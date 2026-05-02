package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/user/live-cdn/internal/common"
	"github.com/user/live-cdn/internal/crypto"
	"gopkg.in/yaml.v3"
)

// Config holds agent configuration
type Config struct {
	NodeID        string `yaml:"node_id"`
	PublicIP      string `yaml:"public_ip"`
	Port          int    `yaml:"port"`
	Region        string `yaml:"region"`
	ISP           string `yaml:"isp"`
	BWLimit       int64  `yaml:"bw_limit"`
	Domain        string `yaml:"domain"`
	Protocol      string `yaml:"protocol"`
	WsPath        string `yaml:"ws_path"`
	TLSEnabled    bool   `yaml:"tls_enabled"`
	ControllerURL string `yaml:"controller_url"`
	RegToken      string `yaml:"reg_token"`
	ListenAddr    string `yaml:"listen_addr"`
	DisguiseDir   string `yaml:"disguise_dir"`
}

// AgentService is the edge node service
type AgentService struct {
	cfg        *Config
	relays     map[string]*StreamRelay
	relayMu    sync.RWMutex
	router     *gin.Engine
	upgrader   websocket.Upgrader
	httpClient *http.Client
	currentBW  int64
	users      int
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	if cfg.NodeID == "" {
		hostname, _ := os.Hostname()
		cfg.NodeID = hostname
	}
	if cfg.Port == 0 {
		cfg.Port = 9090
	}
	if cfg.Protocol == "" {
		cfg.Protocol = "ws"
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = fmt.Sprintf(":%d", cfg.Port)
	}
	return cfg, nil
}

func NewAgent(cfg *Config) *AgentService {
	gin.SetMode(gin.ReleaseMode)
	a := &AgentService{
		cfg:    cfg,
		relays: make(map[string]*StreamRelay),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
	a.router = gin.New()
	a.setupRoutes()
	return a
}

func (a *AgentService) setupRoutes() {
	r := a.router

	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET", "POST", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
		MaxAge:           12 * time.Hour,
	}))
	r.Use(gin.Recovery())

	// Disguise website
	if a.cfg.DisguiseDir != "" {
		r.Static("/static", a.cfg.DisguiseDir)
		r.GET("/", func(c *gin.Context) {
			c.File(a.cfg.DisguiseDir + "/index.html")
		})
	} else {
		r.GET("/", a.handleDisguisePage)
		r.GET("/about", a.handleDisguisePage)
		r.GET("/archive", a.handleDisguisePage)
	}

	// Live streaming endpoints (obfuscated paths)
	wsPath := a.cfg.WsPath
	if wsPath == "" {
		wsPath = "/ws/live"
	}
	r.GET(wsPath, a.handleWebSocket)

	// HLS fallback
	r.GET("/live/:stream/stream.m3u8", a.handleHLSM3U8)
	r.GET("/live/:stream/:segment.ts", a.handleHLSTS)

	// Internal health
	r.GET("/health", a.handleHealth)
}

func (a *AgentService) handleDisguisePage(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(200, `<!DOCTYPE html><html><head><meta charset="utf-8">
<title>My Personal Blog</title><style>body{font-family:Georgia,serif;max-width:800px;margin:40px auto;padding:0 20px;color:#333}
h1{color:#2c3e50}a{color:#3498db}nav{margin:20px 0;padding:10px 0;border-top:1px solid #eee;border-bottom:1px solid #eee}
nav a{margin-right:15px;text-decoration:none}</style></head>
<body><h1>My Personal Blog</h1><nav><a href="/">Home</a><a href="/about">About</a>
<a href="/archive">Archive</a></nav><h2>Latest Posts</h2>
<p>Nothing here yet. Check back later!</p><footer style="margin-top:60px;color:#999;font-size:0.8em">
<p>&copy; 2024 My Blog. All rights reserved.</p></footer></body></html>`)
}

func (a *AgentService) handleWebSocket(c *gin.Context) {
	conn, err := a.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("[Agent] WebSocket upgrade failed: %v", err)
		return
	}

	// First message: handshake with stream_key
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return
	}

	var handshake struct {
		StreamKey string `json:"stream_key"`
		Token     string `json:"token"`
	}
	if err := json.Unmarshal(msg, &handshake); err != nil {
		conn.WriteJSON(map[string]string{"error": "invalid handshake"})
		conn.Close()
		return
	}

	relay := a.getOrCreateRelay(handshake.StreamKey)
	if relay == nil {
		conn.WriteJSON(map[string]string{"error": "stream not available"})
		conn.Close()
		return
	}

	conn.SetReadDeadline(time.Time{})
	relay.HandleWebSocket(conn)
}

func (a *AgentService) handleHLSM3U8(c *gin.Context) {
	streamKey := c.Param("stream")
	relay := a.getRelay(streamKey)
	if relay == nil {
		c.Status(404)
		return
	}
	c.Header("Content-Type", "application/vnd.apple.mpegurl")
	c.Header("Cache-Control", "no-cache")
	c.Header("Access-Control-Allow-Origin", "*")
	relay.HandleHLS(&hlsWriter{c: c}, "stream.m3u8")
}

func (a *AgentService) handleHLSTS(c *gin.Context) {
	streamKey := c.Param("stream")
	segment := c.Param("segment") + ".ts"
	relay := a.getRelay(streamKey)
	if relay == nil {
		c.Status(404)
		return
	}
	c.Header("Content-Type", "video/mp2t")
	c.Header("Access-Control-Allow-Origin", "*")
	relay.HandleHLS(&hlsWriter{c: c}, segment)
}

func (a *AgentService) handleHealth(c *gin.Context) {
	a.relayMu.RLock()
	activeRelays := len(a.relays)
	totalUsers := 0
	for _, r := range a.relays {
		totalUsers += r.ClientCount()
	}
	a.relayMu.RUnlock()

	c.JSON(200, gin.H{
		"status":        "ok",
		"node_id":       a.cfg.NodeID,
		"active_relays": activeRelays,
		"bw_used":       a.currentBW,
		"users":         totalUsers,
	})
}

// hlsWriter adapts gin.Context to the relay's HLS interface
type hlsWriter struct {
	c *gin.Context
}

func (w *hlsWriter) WriteHeader(code int) { w.c.Status(code) }
func (w *hlsWriter) Write(data []byte) int {
	w.c.Data(200, "", data)
	return len(data)
}
func (w *hlsWriter) Header() http.Header { return w.c.Writer.Header() }

// --- Relay management ---

func (a *AgentService) getOrCreateRelay(streamKey string) *StreamRelay {
	a.relayMu.RLock()
	relay, ok := a.relays[streamKey]
	a.relayMu.RUnlock()

	if ok && relay.IsRunning() {
		return relay
	}

	// Create new relay with stream info from controller
	streamInfo, err := a.fetchStreamInfo(streamKey)
	if err != nil {
		log.Printf("[Agent] Failed to fetch stream info: %v", err)
		return nil
	}

	keyInfo, err := crypto.KeyFromBase64(
		streamInfo.EncryptKey, streamInfo.EncryptIV,
		crypto.CipherSuite(streamInfo.CipherSuite),
	)
	if err != nil {
		log.Printf("[Agent] Failed to parse key info: %v", err)
		return nil
	}

	relay = NewStreamRelay(streamKey, streamInfo.OriginURL, keyInfo)
	relay.Start()

	a.relayMu.Lock()
	a.relays[streamKey] = relay
	a.relayMu.Unlock()

	return relay
}

func (a *AgentService) getRelay(streamKey string) *StreamRelay {
	a.relayMu.RLock()
	defer a.relayMu.RUnlock()
	return a.relays[streamKey]
}

func (a *AgentService) fetchStreamInfo(streamKey string) (*common.StreamInfo, error) {
	// In production, this would query the controller API
	// For now, construct from known origin address
	originAddr := "http://origin:8080"
	if a.cfg.ControllerURL != "" {
		originAddr = a.cfg.ControllerURL
	}
	return &common.StreamInfo{
		StreamKey:   streamKey,
		OriginURL:   fmt.Sprintf("%s/live/%s/stream.m3u8", originAddr, streamKey),
		EncryptKey:  "",
		CipherSuite: "chacha20-poly1305",
		IsLive:      true,
	}, nil
}

// --- Heartbeat ---

func (a *AgentService) heartbeatLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		a.relayMu.RLock()
		totalUsers := 0
		rtt := 0
		for _, r := range a.relays {
			totalUsers += r.ClientCount()
			if rtt == 0 {
				rtt = r.MeasureRTT()
			}
		}
		a.relayMu.RUnlock()
		a.users = totalUsers

		hb := common.HeartbeatRequest{
			NodeID:      a.cfg.NodeID,
			BWUsed:      a.currentBW,
			BWLimit:     a.cfg.BWLimit,
			OnlineUsers: a.users,
			RTT:         rtt,
			LossRate:    0,
		}

		jsonData, _ := json.Marshal(hb)
		url := fmt.Sprintf("%s/api/agent/heartbeat", a.cfg.ControllerURL)
		resp, err := a.httpClient.Post(url, "application/json", bytes.NewReader(jsonData))
		if err != nil {
			log.Printf("[Agent] Heartbeat failed: %v", err)
			continue
		}
		resp.Body.Close()
	}
}

// RegisterWithController registers this agent with the controller
func (a *AgentService) RegisterWithController() error {
	reg := common.RegisterRequest{
		NodeID:     a.cfg.NodeID,
		PublicIP:   a.cfg.PublicIP,
		Port:       a.cfg.Port,
		Region:     a.cfg.Region,
		ISP:        a.cfg.ISP,
		BWLimit:    a.cfg.BWLimit,
		Domain:     a.cfg.Domain,
		Protocol:   a.cfg.Protocol,
		WsPath:     a.cfg.WsPath,
		TLSEnabled: a.cfg.TLSEnabled,
		Token:      a.cfg.RegToken,
	}

	jsonData, err := json.Marshal(reg)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/api/agent/register", a.cfg.ControllerURL)
	resp, err := a.httpClient.Post(url, "application/json", bytes.NewReader(jsonData))
	if err != nil {
		return fmt.Errorf("register failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("register returned status %d", resp.StatusCode)
	}

	log.Printf("[Agent] Registered with controller at %s", a.cfg.ControllerURL)
	return nil
}

// Run starts the agent service
func (a *AgentService) Run() error {
	if err := a.RegisterWithController(); err != nil {
		log.Printf("[Agent] Warning: registration failed: %v (will retry in heartbeat)", err)
	}

	go a.heartbeatLoop()

	log.Printf("[Agent] Starting on %s (node=%s region=%s isp=%s)",
		a.cfg.ListenAddr, a.cfg.NodeID, a.cfg.Region, a.cfg.ISP)

	return a.router.Run(a.cfg.ListenAddr)
}
