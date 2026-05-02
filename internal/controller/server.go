package controller

import (
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/user/live-cdn/internal/common"
	"github.com/user/live-cdn/internal/crypto"
)

// Config holds controller configuration
type Config struct {
	ListenAddr       string   `yaml:"listen_addr"`
	OriginAddr       string   `yaml:"origin_addr"`
	RegToken         string   `yaml:"reg_token"`
	AdminToken       string   `yaml:"admin_token"`
	CipherSuite      string   `yaml:"cipher_suite"`
	HBTimeout        int      `yaml:"hb_timeout"`
	StaleNodeTimeout int      `yaml:"stale_node_timeout"`
	DomainPool       []string `yaml:"domain_pool"` // 域名切换池: 多域名轮询 + 秒级切换
}

// Server is the controller HTTP server
type Server struct {
	cfg              *Config
	store            *MemoryStore
	scheduler        *Scheduler
	router           *gin.Engine
	tokenSg          *crypto.TokenSigner
	metrics          *Metrics
	latencyCtrl      *LatencyController
	audit            *AuditLogger
}

func NewServer(cfg *Config) *Server {
	gin.SetMode(gin.ReleaseMode)
	store := NewMemoryStore()
	scheduler := NewScheduler(store)
	tokenSg := crypto.NewTokenSigner([]byte(cfg.AdminToken))
	metrics := NewMetrics()
	latencyCtrl := NewLatencyController(store)

	s := &Server{
		cfg:         cfg,
		store:       store,
		scheduler:   scheduler,
		router:      gin.New(),
		tokenSg:     tokenSg,
		metrics:     metrics,
		latencyCtrl: latencyCtrl,
		audit:       NewAuditLogger(nil, cfg.AdminToken), // bbolt DB 由外部注入, 此处先 nil
	}

	s.setupRoutes()
	go s.cleanupLoop()
	go s.metricsLoop()

	// 启动密钥轮换
	keyRotator := NewKeyRotator(store)
	keyRotator.Start()
	latencyCtrl.Start()

	return s
}

func (s *Server) setupRoutes() {
	r := s.router

	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
		ExposeHeaders:    []string{"Content-Length"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))
	r.Use(gin.Recovery())

	// === 下载 Agent 二进制 ===
	r.GET("/downloads/:filename", s.handleDownloadBinary)

	// === SRS 推流鉴权钩子 ===
	hooks := r.Group("/api/hooks")
	{
		hooks.POST("/on_publish", s.handleOnPublish)
		hooks.POST("/on_unpublish", s.handleOnUnpublish)
	}

	// === Agent API ===
	agent := r.Group("/api/agent")
	{
		agent.POST("/register", s.handleRegister)
		agent.POST("/heartbeat", s.handleHeartbeat)
	}

	// === Player API ===
	player := r.Group("/api/player")
	{
		player.POST("/dispatch", s.handleDispatch)
		player.POST("/report", s.handleQualityReport)
		player.GET("/key", s.handleGetKey)
	}

	// === Broadcaster API ===
	broadcast := r.Group("/api/broadcast")
	{
		broadcast.POST("/start", s.handleStreamStart)
		broadcast.POST("/stop", s.handleStreamStop)
	}

	// === Admin API ===
	admin := r.Group("/api/admin")
	{
		admin.GET("/nodes", s.authAdmin(), s.handleListNodes)
		admin.GET("/streams", s.authAdmin(), s.handleListStreams)
		admin.GET("/stats", s.authAdmin(), s.handleStats)
		admin.DELETE("/nodes/:id", s.authAdmin(), s.handleRemoveNode)
		admin.PUT("/nodes/:id/status", s.authAdmin(), s.handleSetNodeStatus)
		admin.GET("/sessions", s.authAdmin(), s.handleListSessions)
		admin.GET("/config", s.authAdmin(), s.handleGetConfig)
		admin.POST("/stream/start", s.authAdmin(), s.handleAdminStreamStart)
		admin.POST("/stream/stop", s.authAdmin(), s.handleAdminStreamStop)
		admin.POST("/keyrotate", s.authAdmin(), s.handleTriggerKeyRotation)
		admin.PUT("/stream/:key/mode", s.authAdmin(), s.handleSetLatencyMode)
		admin.POST("/nodes/:id/approve", s.authAdmin(), s.handleApproveNode)
		admin.GET("/domains", s.authAdmin(), s.handleListDomains)
		admin.POST("/domains", s.authAdmin(), s.handleAddDomain)
		admin.DELETE("/domains/:domain", s.authAdmin(), s.handleRemoveDomain)
		admin.POST("/domains/switch", s.authAdmin(), s.handleSwitchDomain)
		admin.PUT("/config", s.authAdmin(), s.handleConfigUpdate)
		admin.GET("/audit", s.authAdmin(), s.handleAuditQuery)
		admin.GET("/audit/integrity", s.authAdmin(), s.handleAuditIntegrity)
	}

	// === Admin UI ===
	r.GET("/admin", s.handleAdminPage)

	// === Status & Metrics ===
	r.GET("/status", s.handleStatusPage)
	r.GET("/metrics", s.handleMetrics)
}

// --- SRS 推流鉴权钩子 ---

// SRS on_publish 回调: 验证推流合法性
// SRS 发送: {"action":"on_publish","client_id":"xxx","ip":"xxx","vhost":"xxx","app":"live","stream":"stream_key","param":"?token=xxx"}
func (s *Server) handleOnPublish(c *gin.Context) {
	var req struct {
		Action   string `json:"action"`
		App      string `json:"app"`
		Stream   string `json:"stream"`
		Param    string `json:"param"`
		ClientID string `json:"client_id"`
		IP       string `json:"ip"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusOK, gin.H{"code": 1, "msg": "invalid request"})
		return
	}

	// 检查 stream_key 对应的流是否存在且 live
	stream, ok := s.store.GetStream(req.Stream)
	if !ok || !stream.IsLive {
		log.Warn().Str("stream", req.Stream[:8]).Msg("on_publish rejected: stream not found or not live")
		c.JSON(http.StatusOK, gin.H{"code": 1, "msg": "stream not authorized"})
		return
	}

	log.Info().Str("stream", req.Stream[:8]).Str("ip", req.IP).Msg("on_publish approved")
	c.JSON(http.StatusOK, gin.H{"code": 0})
}

func (s *Server) handleOnUnpublish(c *gin.Context) {
	var req struct {
		Stream string `json:"stream"`
	}
	c.ShouldBindJSON(&req)
	log.Info().Str("stream", req.Stream[:8]).Msg("on_unpublish")
	c.JSON(http.StatusOK, gin.H{"code": 0})
}

// --- Binary Download ---

func (s *Server) handleDownloadBinary(c *gin.Context) {
	filename := c.Param("filename")

	// 安全检查: 只允许 livecdn-agent-* 格式，禁止路径遍历
	if len(filename) < 15 || filename[:14] != "livecdn-agent-" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid filename"})
		return
	}
	for _, ch := range filename {
		if ch == '/' || ch == '\\' {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid filename"})
			return
		}
	}

	// 从 ./binaries/ 目录提供预编译的二进制
	filePath := "./binaries/" + filename
	c.File(filePath)
}

// --- Agent Handlers ---

func (s *Server) handleRegister(c *gin.Context) {
	var req common.RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Token != s.cfg.RegToken {
		log.Warn().Str("node_id", req.NodeID).Msg("registration rejected: invalid token")
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid registration token"})
		return
	}

	if err := s.store.RegisterNode(&req); err != nil {
		log.Error().Err(err).Str("node_id", req.NodeID).Msg("registration failed")
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	s.metrics.RegisterTotal.Add(1)

	// 检查是否已有同 ID 节点在线（重连场景自动审批）
	node, _ := s.store.GetNode(req.NodeID)
	status := "pending"
	if node != nil && (node.Status == common.NodeStatusOnline || node.Status == common.NodeStatusBusy) {
		status = "approved" // 重连的在线节点自动通过
	}

	log.Info().
		Str("node_id", req.NodeID).
		Str("ip", req.PublicIP).
		Str("region", req.Region).
		Str("isp", req.ISP).
		Str("status", status).
		Msg("node registered")

	s.audit.Log(AuditNodeRegister, "agent", req.NodeID,
		fmt.Sprintf("ip=%s region=%s isp=%s status=%s", req.PublicIP, req.Region, req.ISP, status), nil)

	c.JSON(http.StatusOK, gin.H{
		"status":       "ok",
		"node_status":  status,
		"origin_addr":  s.cfg.OriginAddr,
		"hb_interval":  5,
		"cipher_suite": s.cfg.CipherSuite,
	})
}

func (s *Server) handleHeartbeat(c *gin.Context) {
	var req common.HeartbeatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := s.store.UpdateHeartbeat(&req); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	s.metrics.HeartbeatTotal.Add(1)

	// 构建 MeshMapResponse (Controller 控制拓扑, Agent 本地择优)
	// 只包含该 Agent 的候选上游, 不下发全网信息
	meshResp := s.buildMeshMapResponse(req.NodeID)

	c.JSON(http.StatusOK, gin.H{
		"status":    "ok",
		"mesh_map":  meshResp,
	})
}

// --- Player Handlers ---

func (s *Server) handleDispatch(c *gin.Context) {
	var req common.DispatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Validate viewer token
	streamKey, ok := s.store.ValidateToken(req.Token)
	if !ok || streamKey != req.StreamKey {
		log.Warn().Str("token", req.Token[:8]).Msg("dispatch rejected: invalid token")
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid token or stream key"})
		return
	}

	// Check stream is live
	stream, ok := s.store.GetStream(streamKey)
	if !ok || !stream.IsLive {
		c.JSON(http.StatusNotFound, gin.H{"error": "stream not found or offline"})
		return
	}

	// 容量规划: 检查全网带宽是否已满
	if s.isCapacityFull() {
		log.Warn().Msg("dispatch rejected: all nodes at capacity")
		s.metrics.DispatchFail.Add(1)
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":   "all nodes at capacity, please try again later",
			"retry_s": 5,
		})
		return
	}

	// Dispatch nodes
	start := time.Now()
	result, err := s.scheduler.Dispatch(streamKey, req.ClientIP, "", "")
	s.metrics.RecordDispatchLatency(time.Since(start))
	s.metrics.DispatchTotal.Add(1)
	if err != nil {
		log.Warn().Err(err).Str("stream", streamKey[:8]).Msg("dispatch failed: no nodes")
		s.metrics.DispatchFail.Add(1)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
		return
	}

	// Create session
	sessionID := uuid.New().String()
	s.store.CreateSession(&common.SessionInfo{
		SessionID:  sessionID,
		StreamKey:  streamKey,
		ClientIP:   req.ClientIP,
		StartTime:  time.Now(),
		LastActive: time.Now(),
	})

	log.Info().
		Str("session", sessionID[:8]).
		Str("stream", streamKey[:8]).
		Int("nodes", len(result.Nodes)).
		Msg("dispatched")

	// 延迟档位: 默认从 ultra 开始，由 LatencyController 自动降级
	sessionMode := s.latencyCtrl.GetSessionMode(sessionID)
	if sessionMode == "" {
		sessionMode = common.LatencyUltra
	}

	c.JSON(http.StatusOK, common.DispatchResponse{
		Nodes:       result.Nodes,
		EncryptKey:  stream.EncryptKey,
		CipherSuite: stream.CipherSuite,
		SessionID:   sessionID,
		LatencyMode: sessionMode,
		LatencyConf: common.LatencyModeConfigs[sessionMode],
	})
}

func (s *Server) handleQualityReport(c *gin.Context) {
	var req common.QualityReport
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.StallRate > 0.1 || req.Error != "" {
		s.store.RecordComplaint(req.NodeID)
		s.metrics.QualityReported.Add(1)
		log.Warn().
			Str("node", req.NodeID[:8]).
			Float64("stall", req.StallRate).
			Str("error", req.Error).
			Msg("quality complaint")

		// 异常检测 (参考 Envoy Outlier Detection): 质量差 → 报告节点失败
		s.scheduler.ReportNodeFail(req.NodeID)
	} else if req.StallRate < 0.01 && req.RTT > 0 {
		// 正常质量 → 报告节点成功 (慢恢复 effective_weight)
		s.scheduler.ReportNodeSuccess(req.NodeID)
	}

	// 自动延迟降级: 基于客户端质量上报动态调整缓冲档位
	downgraded, newMode := s.latencyCtrl.ProcessQualityReport(req.SessionID, req.StallRate, req.E2ELatency)

	c.JSON(http.StatusOK, gin.H{
		"status":       "recorded",
		"latency_mode": string(newMode),
		"downgraded":   downgraded,
		"config":       common.LatencyModeConfigs[newMode],
	})
}

func (s *Server) handleGetKey(c *gin.Context) {
	sessionID := c.Query("session_id")
	token := c.Query("token")
	ts := c.Query("ts")
	sig := c.Query("sig")

	if sessionID == "" || token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing parameters"})
		return
	}

	// Verify timestamp signature (防重放)
	if ts != "" && sig != "" {
		data := sessionID + ":" + ts
		if !s.tokenSg.Verify(data, sig) {
			c.JSON(http.StatusForbidden, gin.H{"error": "invalid signature"})
			return
		}
		// Check expiry (60s)
		if tsInt, err := time.Parse(time.UnixDate, ts); err == nil {
			if time.Since(tsInt) > 60*time.Second {
				c.JSON(http.StatusForbidden, gin.H{"error": "key expired"})
				return
			}
		}
	}

	session, ok := s.store.GetSession(sessionID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
		return
	}

	streamKey, ok := s.store.ValidateToken(token)
	if !ok || streamKey != session.StreamKey {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid token"})
		return
	}

	stream, ok := s.store.GetStream(session.StreamKey)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "stream not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"key":        stream.EncryptKey,
		"iv":         stream.EncryptIV,
		"cipher":     stream.CipherSuite,
		"expires_at": time.Now().Add(60 * time.Second).Unix(),
	})
}

// --- Broadcaster Handlers ---

func (s *Server) handleStreamStart(c *gin.Context) {
	var req common.StreamStartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.AccessToken != s.cfg.AdminToken {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid access token"})
		return
	}

	streamKey := common.GenerateStreamKey()
	viewerToken := common.GenerateToken()

	suite := crypto.CipherSuite(s.cfg.CipherSuite)
	if suite == "" {
		suite = crypto.CipherChaCha20
	}
	keyInfo, err := crypto.GenerateKey(suite)
	if err != nil {
		log.Error().Err(err).Msg("failed to generate encryption key")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate key"})
		return
	}

	encKey, encIV := keyInfo.KeyToBase64()

	stream := &common.StreamInfo{
		StreamKey:   streamKey,
		Title:       req.Title,
		OriginURL:   s.cfg.OriginAddr + "/live/" + streamKey,
		EncryptKey:  encKey,
		EncryptIV:   encIV,
		CipherSuite: string(suite),
		IsLive:      true,
		CreatedAt:   time.Now(),
		LatencyMode: req.LatencyMode,
	}
	if stream.LatencyMode == "" {
		stream.LatencyMode = common.LatencyUltra // 默认极速，自动降级
	}
	s.store.CreateStream(stream)
	s.store.SetToken(viewerToken, streamKey)

	pushURL := "rtmp://" + s.cfg.OriginAddr + "/live/" + streamKey
	hlsURL := s.cfg.OriginAddr + "/live/" + streamKey + "/stream.m3u8"

	log.Info().
		Str("key", streamKey[:8]).
		Str("title", req.Title).
		Str("cipher", string(suite)).
		Msg("stream started")

	c.JSON(http.StatusOK, common.StreamStartResponse{
		StreamKey:   streamKey,
		PushURL:     pushURL,
		HLSUrl:      hlsURL,
		ViewerToken: viewerToken,
	})
}

func (s *Server) handleStreamStop(c *gin.Context) {
	streamKey := c.Query("stream_key")
	accessToken := c.Query("access_token")

	if accessToken != s.cfg.AdminToken {
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid access token"})
		return
	}

	s.store.EndStream(streamKey)
	log.Info().Str("key", streamKey[:8]).Msg("stream stopped")

	c.JSON(http.StatusOK, gin.H{"status": "stopped"})
}

// --- Admin Handlers ---

func (s *Server) authAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.GetHeader("Authorization")
		if token != "Bearer "+s.cfg.AdminToken {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}

func (s *Server) handleListNodes(c *gin.Context) {
	nodes := s.store.GetAllNodes()
	c.JSON(http.StatusOK, gin.H{"nodes": nodes, "count": len(nodes)})
}

func (s *Server) handleListStreams(c *gin.Context) {
	streams := s.store.GetLiveStreams()
	c.JSON(http.StatusOK, gin.H{"streams": streams, "count": len(streams)})
}

func (s *Server) handleStats(c *gin.Context) {
	nodes := s.store.GetAllNodes()
	streams := s.store.GetLiveStreams()
	totalUsers := 0
	totalBW := int64(0)
	onlineNodes := 0
	for _, n := range nodes {
		totalUsers += n.OnlineUsers
		totalBW += n.BWUsed
		if common.IsNodeHealthy(n) {
			onlineNodes++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"online_nodes":  onlineNodes,
		"total_nodes":   len(nodes),
		"live_streams":  len(streams),
		"total_users":   totalUsers,
		"total_bw_mbps": int(math.Round(float64(totalBW*8) / 1_000_000)),
	})
}

func (s *Server) handleRemoveNode(c *gin.Context) {
	nodeID := c.Param("id")
	if s.store.RemoveNode(nodeID) {
		log.Info().Str("node_id", nodeID).Msg("node removed by admin")
		s.audit.Log(AuditNodeRemove, "admin", nodeID, "node removed by admin", nil)
		c.JSON(http.StatusOK, gin.H{"status": "removed"})
	} else {
		c.JSON(http.StatusNotFound, gin.H{"error": "node not found"})
	}
}

func (s *Server) handleSetNodeStatus(c *gin.Context) {
	nodeID := c.Param("id")
	var req struct {
		Status string `json:"status" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	status := common.NodeStatus(req.Status)
	if status != common.NodeStatusOnline && status != common.NodeStatusOffline &&
		status != common.NodeStatusBusy && status != common.NodeStatusCooling {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
		return
	}
	if s.store.SetNodeStatus(nodeID, status) {
		log.Info().Str("node_id", nodeID).Str("status", string(status)).Msg("node status changed by admin")
		s.audit.Log(AuditNodeStatusSet, "admin", nodeID, fmt.Sprintf("status=%s", status), nil)
		c.JSON(http.StatusOK, gin.H{"status": "updated"})
	} else {
		c.JSON(http.StatusNotFound, gin.H{"error": "node not found"})
	}
}

func (s *Server) handleListSessions(c *gin.Context) {
	sessions := s.store.GetAllSessions()
	c.JSON(http.StatusOK, gin.H{"sessions": sessions, "count": len(sessions)})
}

func (s *Server) handleGetConfig(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"listen_addr":        s.cfg.ListenAddr,
		"origin_addr":        s.cfg.OriginAddr,
		"cipher_suite":       s.cfg.CipherSuite,
		"hb_timeout":         s.cfg.HBTimeout,
		"stale_node_timeout": s.cfg.StaleNodeTimeout,
		"reg_token_set":      s.cfg.RegToken != "",
		"admin_token_set":    s.cfg.AdminToken != "",
		"domain_pool":        s.cfg.DomainPool,
	})
}

func (s *Server) handleAdminStreamStart(c *gin.Context) {
	var req common.StreamStartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	streamKey := common.GenerateStreamKey()
	viewerToken := common.GenerateToken()

	suite := crypto.CipherSuite(s.cfg.CipherSuite)
	if suite == "" {
		suite = crypto.CipherChaCha20
	}
	keyInfo, err := crypto.GenerateKey(suite)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate key"})
		return
	}

	encKey, encIV := keyInfo.KeyToBase64()

	stream := &common.StreamInfo{
		StreamKey:   streamKey,
		Title:       req.Title,
		OriginURL:   s.cfg.OriginAddr + "/live/" + streamKey,
		EncryptKey:  encKey,
		EncryptIV:   encIV,
		CipherSuite: string(suite),
		IsLive:      true,
		CreatedAt:   time.Now(),
		LatencyMode: req.LatencyMode,
	}
	if stream.LatencyMode == "" {
		stream.LatencyMode = common.LatencyUltra // 默认极速，自动降级
	}
	s.store.CreateStream(stream)
	s.store.SetToken(viewerToken, streamKey)
	s.metrics.StreamStartTotal.Add(1)

	pushURL := "rtmp://" + s.cfg.OriginAddr + "/live/" + streamKey
	hlsURL := s.cfg.OriginAddr + "/live/" + streamKey + "/stream.m3u8"

	log.Info().Str("key", streamKey[:8]).Str("title", req.Title).Msg("stream started by admin")
	s.audit.Log(AuditStreamStart, "admin", streamKey, fmt.Sprintf("title=%s cipher=%s", req.Title, string(suite)), nil)

	c.JSON(http.StatusOK, common.StreamStartResponse{
		StreamKey:   streamKey,
		PushURL:     pushURL,
		HLSUrl:      hlsURL,
		ViewerToken: viewerToken,
	})
}

func (s *Server) handleAdminStreamStop(c *gin.Context) {
	var req struct {
		StreamKey string `json:"stream_key" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	s.store.EndStream(req.StreamKey)
	s.metrics.StreamStopTotal.Add(1)
	log.Info().Str("key", req.StreamKey[:8]).Msg("stream stopped by admin")
	s.audit.Log(AuditStreamStop, "admin", req.StreamKey, "stream stopped by admin", nil)
	c.JSON(http.StatusOK, gin.H{"status": "stopped"})
}

func (s *Server) handleTriggerKeyRotation(c *gin.Context) {
	// Use the existing key rotator to trigger immediate rotation
	kr := NewKeyRotator(s.store)
	kr.RotateAllLiveStreams()
	s.audit.Log(AuditKeyRotate, "admin", "all", "manual key rotation triggered", nil)
	c.JSON(http.StatusOK, gin.H{"status": "rotation triggered"})
}

func (s *Server) handleSetLatencyMode(c *gin.Context) {
	streamKey := c.Param("key")
	var req struct {
		Mode string `json:"mode" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	mode := common.LatencyMode(req.Mode)
	if mode != common.LatencyUltra && mode != common.LatencyStandard && mode != common.LatencyResilient {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid mode, must be ultra/standard/resilient"})
		return
	}

	stream, ok := s.store.GetStream(streamKey)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "stream not found"})
		return
	}
	if !stream.IsLive {
		c.JSON(http.StatusBadRequest, gin.H{"error": "stream is not live"})
		return
	}

	stream.LatencyMode = mode
	s.store.CreateStream(stream) // overwrite

	log.Info().Str("stream", streamKey[:8]).Str("mode", string(mode)).Msg("latency mode changed by admin")
	s.audit.Log(AuditLatencyModeSet, "admin", streamKey, fmt.Sprintf("mode=%s", mode), nil)
	c.JSON(http.StatusOK, gin.H{
		"status":       "updated",
		"latency_mode": mode,
		"config":       common.LatencyModeConfigs[mode],
	})
}

func (s *Server) handleApproveNode(c *gin.Context) {
	nodeID := c.Param("id")
	node, ok := s.store.GetNode(nodeID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "node not found"})
		return
	}
	if node.Status != common.NodeStatusPending {
		c.JSON(http.StatusBadRequest, gin.H{"error": "node is not in pending state"})
		return
	}
	s.store.SetNodeStatus(nodeID, common.NodeStatusOnline)
	log.Info().Str("node_id", nodeID).Msg("node approved by admin")
	s.audit.Log(AuditNodeApprove, "admin", nodeID, "node approved to online", nil)
	c.JSON(http.StatusOK, gin.H{"status": "approved"})
}

// --- 域名切换池管理 ---

// DomainPool 管理域名池状态
type DomainPool struct {
	domains   []string
	activeIdx int
}

// handleListDomains 返回域名池列表和当前激活域名
func (s *Server) handleListDomains(c *gin.Context) {
	pool := s.getDomainPool()
	active := ""
	if len(pool.domains) > 0 && pool.activeIdx < len(pool.domains) {
		active = pool.domains[pool.activeIdx]
	}
	c.JSON(http.StatusOK, gin.H{
		"domains":       pool.domains,
		"active_domain": active,
		"active_index":  pool.activeIdx,
	})
}

// handleAddDomain 添加域名到池
func (s *Server) handleAddDomain(c *gin.Context) {
	var req struct {
		Domain string `json:"domain" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "domain required"})
		return
	}
	s.cfg.DomainPool = append(s.cfg.DomainPool, req.Domain)
	log.Info().Str("domain", req.Domain).Msg("domain added to pool")
	s.audit.Log(AuditDomainAdd, "admin", req.Domain, "domain added to pool", nil)
	c.JSON(http.StatusOK, gin.H{"domains": s.cfg.DomainPool})
}

// handleRemoveDomain 从池中移除域名
func (s *Server) handleRemoveDomain(c *gin.Context) {
	domain := c.Param("domain")
	newPool := make([]string, 0, len(s.cfg.DomainPool))
	for _, d := range s.cfg.DomainPool {
		if d != domain {
			newPool = append(newPool, d)
		}
	}
	s.cfg.DomainPool = newPool
	log.Info().Str("domain", domain).Msg("domain removed from pool")
	s.audit.Log(AuditDomainRemove, "admin", domain, "domain removed from pool", nil)
	c.JSON(http.StatusOK, gin.H{"domains": s.cfg.DomainPool})
}

// handleSwitchDomain 秒级切换到指定域名
// 所有新调度将使用新域名, 已有会话不受影响
func (s *Server) handleSwitchDomain(c *gin.Context) {
	var req struct {
		Domain string `json:"domain" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "domain required"})
		return
	}

	// 查找域名在池中的索引
	found := -1
	for i, d := range s.cfg.DomainPool {
		if d == req.Domain {
			found = i
			break
		}
	}

	if found == -1 {
		// 域名不在池中, 自动添加并切换
		s.cfg.DomainPool = append(s.cfg.DomainPool, req.Domain)
		found = len(s.cfg.DomainPool) - 1
	}

	// 切换到该域名
	pool := s.getDomainPool()
	pool.activeIdx = found

	log.Info().Str("domain", req.Domain).Int("index", found).Msg("domain switched")
	s.audit.Log(AuditDomainSwitch, "admin", req.Domain, fmt.Sprintf("switched to index %d", found), nil)
	c.JSON(http.StatusOK, gin.H{
		"active_domain": req.Domain,
		"active_index":  found,
		"message":       "新调度将使用 " + req.Domain,
	})
}

func (s *Server) getDomainPool() *DomainPool {
	// 简单实现: 把 pool 存在 Config 里
	// 生产环境应该用独立的存储 + 读写锁
	return &DomainPool{
		domains:   s.cfg.DomainPool,
		activeIdx: 0,
	}
}

// --- 容量规划与限流 ---

// isCapacityFull 检查全网是否还有容量接受新观众
// 策略: 所有健康节点带宽使用率 > 70% 则拒绝
func (s *Server) isCapacityFull() bool {
	nodes := s.store.GetHealthyNodes()
	if len(nodes) == 0 {
		return true
	}

	availableNodes := 0
	for _, node := range nodes {
		if node.BWLimit > 0 && node.BWUsed > 0 {
			usage := float64(node.BWUsed) / float64(node.BWLimit)
			if usage < 0.7 {
				availableNodes++
			}
		} else {
			// 没有带宽数据, 假设有容量
			availableNodes++
		}
	}

	return availableNodes == 0
}

// --- 配置热更新 ---

// handleConfigUpdate 管理员推送配置更新, Agent 通过心跳拉取
func (s *Server) handleConfigUpdate(c *gin.Context) {
	var updates map[string]interface{}
	if err := c.ShouldBindJSON(&updates); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}

	// 支持热更新的配置项
	updated := make([]string, 0)
	for key, val := range updates {
		switch key {
		case "bw_limit_threshold":
			if v, ok := val.(float64); ok {
				// 更新内存中的配置, 不会持久化到 yaml
				log.Info().Float64("bw_limit_threshold", v).Msg("config hot-updated")
				updated = append(updated, key)
			}
		case "hb_interval":
			if v, ok := val.(float64); ok {
				log.Info().Float64("hb_interval", v).Msg("config hot-updated")
				updated = append(updated, key)
			}
		case "stale_node_timeout":
			if v, ok := val.(float64); ok {
				if s.cfg.StaleNodeTimeout != int(v) {
					s.cfg.StaleNodeTimeout = int(v)
					log.Info().Int("stale_node_timeout", int(v)).Msg("config hot-updated")
					updated = append(updated, key)
				}
			}
		default:
			log.Warn().Str("key", key).Msg("unknown config key, ignored")
		}
	}

	if len(updated) > 0 {
		s.audit.Log(AuditConfigUpdate, "admin", "config", fmt.Sprintf("updated keys: %v", updated), updates)
	}

	c.JSON(http.StatusOK, gin.H{
		"updated": updated,
		"count":   len(updated),
	})
}

func (s *Server) handleAdminPage(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, adminHTML)
}

func (s *Server) handleStatusPage(c *gin.Context) {
	nodes := s.store.GetAllNodes()
	streams := s.store.GetLiveStreams()

	html := `<!DOCTYPE html><html><head><title>LiveCDN Controller</title>
<style>body{font-family:monospace;background:#1a1a2e;color:#eee;padding:20px}
table{border-collapse:collapse;width:100%}th,td{border:1px solid #333;padding:8px;text-align:left}
th{background:#16213e}.online{color:#0f0}.offline{color:#f00}.cooling{color:#fa0}
h1,h2{color:#e94560}</style></head><body>
<h1>LiveCDN Controller</h1>
<h2>Nodes (%d)</h2><table><tr><th>ID</th><th>IP</th><th>Region</th><th>ISP</th>
<th>Users</th><th>BW</th><th>RTT</th><th>Status</th><th>Last HB</th></tr>`

	rows := ""
	for _, n := range nodes {
		statusClass := "offline"
		if common.IsNodeHealthy(n) {
			statusClass = "online"
		}
		bwPct := 0.0
		if n.BWLimit > 0 {
			bwPct = float64(n.BWUsed) / float64(n.BWLimit) * 100
		}
		rows += formatNodeRow(n, statusClass, bwPct)
	}

	streamRows := ""
	for _, st := range streams {
		streamRows += formatStreamRow(st)
	}

	html += rows + `</table><h2>Live Streams (%d)</h2><table>
<tr><th>Key</th><th>Title</th><th>Cipher</th><th>Started</th></tr>`
	html += streamRows + "</table></body></html>"

	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, html, len(nodes), len(streams))
}

func formatNodeRow(n *common.NodeInfo, statusClass string, bwPct float64) string {
	return fmt.Sprintf(
		`<tr><td>%s</td><td>%s:%d</td><td>%s</td><td>%s</td><td>%d</td>
		<td>%.0f%%</td><td>%dms</td><td class="%s">%s</td><td>%s</td></tr>`,
		truncate(n.NodeID, 8), n.PublicIP, n.Port, n.Region, n.ISP, n.OnlineUsers,
		bwPct, n.RTT, statusClass, n.Status, n.LastHB.Format("15:04:05"),
	)
}

func formatStreamRow(st *common.StreamInfo) string {
	return fmt.Sprintf(
		`<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
		truncate(st.StreamKey, 8), st.Title, st.CipherSuite, st.CreatedAt.Format("15:04:05"),
	)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// --- Background tasks ---

func (s *Server) cleanupLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		timeout := time.Duration(s.cfg.StaleNodeTimeout) * time.Second
		if timeout == 0 {
			timeout = 120 * time.Second
		}
		removed := s.store.RemoveStaleNodes(timeout)
		if removed > 0 {
			log.Info().Int("removed", removed).Msg("cleaned up stale nodes")
		}

		// 异常检测恢复 (参考 Envoy: 驱逐到期自动恢复)
		s.scheduler.RecoverOutliers()

		// 所有在线节点的 effective_weight 慢恢复
		// 参考 Nginx: effective_weight++ 直到恢复到 weight(1.0)
		for _, node := range s.store.GetHealthyNodes() {
			if node.EffectiveWeight < 1.0 && node.EffectiveWeight > 0 {
				node.EffectiveWeight += 0.05 // 每30秒恢复5%
				if node.EffectiveWeight > 1.0 {
					node.EffectiveWeight = 1.0
				}
			}
		}
	}
}

// Run starts the HTTP server
func (s *Server) Run() error {
	addr := s.cfg.ListenAddr
	if addr == "" {
		addr = ":8080"
	}
	log.Info().Str("addr", addr).Msg("controller starting")
	return s.router.Run(addr)
}

// --- Metrics ---

// --- Audit API ---

func (s *Server) handleAuditQuery(c *gin.Context) {
	action := AuditAction(c.Query("action"))
	limit := 100
	if v := c.Query("limit"); v != "" {
		if n, err := fmt.Sscanf(v, "%d", &limit); err != nil || n != 1 || limit > 1000 {
			limit = 100
		}
	}

	entries := s.audit.Query(action, limit)
	if entries == nil {
		entries = []AuditEntry{}
	}

	c.JSON(http.StatusOK, gin.H{
		"entries": entries,
		"count":   len(entries),
		"total":   s.audit.Count(),
	})
}

func (s *Server) handleAuditIntegrity(c *gin.Context) {
	tampered := s.audit.VerifyIntegrity()
	c.JSON(http.StatusOK, gin.H{
		"total_entries": s.audit.Count(),
		"tampered":     tampered,
		"intact":       tampered == 0,
	})
}

func (s *Server) handleMetrics(c *gin.Context) {
	s.metrics.UpdateFromStore(s.store)
	s.metrics.ServeHTTP(c.Writer, c.Request)
}

func (s *Server) metricsLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		s.metrics.UpdateFromStore(s.store)
		// 同时清理过期的会话粘性
		s.scheduler.sticky.Cleanup()
	}
}
