package agent

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/user/live-cdn/internal/common"
	"github.com/user/live-cdn/internal/crypto"
	"github.com/user/live-cdn/internal/protocol"
)

// StreamRelay manages pulling stream from origin and serving to clients
type StreamRelay struct {
	mu          sync.RWMutex
	streamKey   string
	originURL   string // HLS origin URL
	keyInfo     *crypto.KeyInfo
	encryptor   *crypto.StreamEncryptor

	// Ring buffer for recent segments
	segments    []*Segment
	segMu       sync.RWMutex
	maxSegments int

	// Client connections
	clients    map[string]*ClientConn
	clientMu   sync.RWMutex

	// State
	running    bool
	stopCh     chan struct{}
}

type Segment struct {
	Seq       uint32
	Type      string // "m3u8" or "ts"
	Data      []byte // encrypted data
	Duration  float64
	Timestamp time.Time
}

type ClientConn struct {
	ID        string
	Conn      *websocket.Conn
	WriteCh   chan []byte
	CloseCh   chan struct{}
	SeqCursor uint32
}

func NewStreamRelay(streamKey, originURL string, keyInfo *crypto.KeyInfo) *StreamRelay {
	return &StreamRelay{
		streamKey:   streamKey,
		originURL:   originURL,
		keyInfo:     keyInfo,
		encryptor:   crypto.NewStreamEncryptor(keyInfo),
		segments:    make([]*Segment, 0, 30),
		maxSegments: 30,
		clients:     make(map[string]*ClientConn),
		stopCh:      make(chan struct{}),
	}
}

// Start begins pulling from origin
func (sr *StreamRelay) Start() {
	sr.mu.Lock()
	if sr.running {
		sr.mu.Unlock()
		return
	}
	sr.running = true
	sr.mu.Unlock()

	go sr.pullLoop()
	log.Printf("[Relay] Started relay for stream %s from %s", sr.streamKey[:8], sr.originURL)
}

// Stop shuts down the relay
func (sr *StreamRelay) Stop() {
	sr.mu.Lock()
	sr.running = false
	sr.mu.Unlock()

	close(sr.stopCh)

	// Close all clients
	sr.clientMu.Lock()
	for _, c := range sr.clients {
		close(c.CloseCh)
		c.Conn.Close()
	}
	sr.clients = make(map[string]*ClientConn)
	sr.clientMu.Unlock()

	log.Printf("[Relay] Stopped relay for stream %s", sr.streamKey[:8])
}

// IsRunning returns whether the relay is active
func (sr *StreamRelay) IsRunning() bool {
	sr.mu.RLock()
	defer sr.mu.RUnlock()
	return sr.running
}

// AddClient adds a WebSocket client connection
func (sr *StreamRelay) AddClient(conn *websocket.Conn) string {
	clientID := common.GenerateToken()

	client := &ClientConn{
		ID:      clientID,
		Conn:    conn,
		WriteCh: make(chan []byte, 64),
		CloseCh: make(chan struct{}),
	}

	sr.clientMu.Lock()
	sr.clients[clientID] = client
	sr.clientMu.Unlock()

	// Start writer goroutine
	go sr.clientWriter(client)

	// Send recent segments to new client
	sr.sendRecentSegments(client)

	log.Printf("[Relay] Client %s connected to stream %s", clientID[:8], sr.streamKey[:8])
	return clientID
}

// RemoveClient removes a client connection
func (sr *StreamRelay) RemoveClient(clientID string) {
	sr.clientMu.Lock()
	client, ok := sr.clients[clientID]
	if ok {
		close(client.CloseCh)
		delete(sr.clients, clientID)
	}
	sr.clientMu.Unlock()

	if ok {
		client.Conn.Close()
		log.Printf("[Relay] Client %s disconnected from stream %s", clientID[:8], sr.streamKey[:8])
	}
}

// ClientCount returns the number of connected clients
func (sr *StreamRelay) ClientCount() int {
	sr.clientMu.RLock()
	defer sr.clientMu.RUnlock()
	return len(sr.clients)
}

// pullLoop periodically fetches the m3u8 playlist and new TS segments from origin
func (sr *StreamRelay) pullLoop() {
	client := &http.Client{Timeout: 10 * time.Second}
	ticker := time.NewTicker(2 * time.Second) // Poll interval
	defer ticker.Stop()

	var lastM3U8 string
	var seqCounter uint32

	for {
		select {
		case <-sr.stopCh:
			return
		case <-ticker.C:
		}

		// Fetch m3u8 playlist
		m3u8URL := sr.originURL
		resp, err := client.Get(m3u8URL)
		if err != nil {
			log.Printf("[Relay] Failed to fetch m3u8: %v", err)
			continue
		}
		m3u8Data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Printf("[Relay] Failed to read m3u8: %v", err)
			continue
		}

		m3u8Str := string(m3u8Data)
		if m3u8Str == lastM3U8 {
			continue // No new segments
		}
		lastM3U8 = m3u8Str

		// Encrypt and store the m3u8
		encM3U8, err := sr.encryptor.Encrypt(m3u8Data)
		if err != nil {
			log.Printf("[Relay] Failed to encrypt m3u8: %v", err)
			continue
		}
		sr.addSegment(&Segment{
			Seq:       seqCounter,
			Type:      "m3u8",
			Data:      encM3U8,
			Duration:  0,
			Timestamp: time.Now(),
		})
		seqCounter++

		// Parse m3u8 for TS segment URLs and fetch new ones
		tsURLs := parseM3U8ForTS(m3u8Str, sr.originURL)
		for _, tsURL := range tsURLs {
			resp, err := client.Get(tsURL)
			if err != nil {
				log.Printf("[Relay] Failed to fetch TS %s: %v", tsURL, err)
				continue
			}
			tsData, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				continue
			}

			encTS, err := sr.encryptor.Encrypt(tsData)
			if err != nil {
				continue
			}

			sr.addSegment(&Segment{
				Seq:       seqCounter,
				Type:      "ts",
				Data:      encTS,
				Duration:  2.0, // typical segment duration
				Timestamp: time.Now(),
			})
			seqCounter++
		}

		// Broadcast latest m3u8 to all clients
		sr.broadcastLatestM3U8()
	}
}

// addSegment adds a segment to the ring buffer
func (sr *StreamRelay) addSegment(seg *Segment) {
	sr.segMu.Lock()
	defer sr.segMu.Unlock()

	sr.segments = append(sr.segments, seg)
	if len(sr.segments) > sr.maxSegments {
		sr.segments = sr.segments[len(sr.segments)-sr.maxSegments:]
	}
}

// getLatestM3U8 returns the most recent m3u8 segment
func (sr *StreamRelay) getLatestM3U8() *Segment {
	sr.segMu.RLock()
	defer sr.segMu.RUnlock()

	for i := len(sr.segments) - 1; i >= 0; i-- {
		if sr.segments[i].Type == "m3u8" {
			return sr.segments[i]
		}
	}
	return nil
}

// sendRecentSegments sends the latest m3u8 to a new client
func (sr *StreamRelay) sendRecentSegments(client *ClientConn) {
	seg := sr.getLatestM3U8()
	if seg == nil {
		return
	}

	frame, err := protocol.MarshalDataPayload(seg.Seq, seg.Type, seg.Data, seg.Duration)
	if err != nil {
		return
	}
	data, err := protocol.MarshalFrame(frame)
	if err != nil {
		return
	}

	select {
	case client.WriteCh <- data:
	default:
		// Channel full, drop
	}
}

// broadcastLatestM3U8 sends the latest m3u8 to all clients
func (sr *StreamRelay) broadcastLatestM3U8() {
	seg := sr.getLatestM3U8()
	if seg == nil {
		return
	}

	frame, err := protocol.MarshalDataPayload(seg.Seq, seg.Type, seg.Data, seg.Duration)
	if err != nil {
		return
	}
	data, err := protocol.MarshalFrame(frame)
	if err != nil {
		return
	}

	sr.clientMu.RLock()
	defer sr.clientMu.RUnlock()

	for _, client := range sr.clients {
		select {
		case client.WriteCh <- data:
		default:
			// Channel full, skip
		}
	}
}

// clientWriter writes data to a client WebSocket connection
func (sr *StreamRelay) clientWriter(client *ClientConn) {
	for {
		select {
		case <-client.CloseCh:
			return
		case data, ok := <-client.WriteCh:
			if !ok {
				return
			}
			err := client.Conn.WriteMessage(websocket.BinaryMessage, data)
			if err != nil {
				sr.RemoveClient(client.ID)
				return
			}
		}
	}
}

// parseM3U8ForTS extracts TS segment URLs from an m3u8 playlist
func parseM3U8ForTS(m3u8, baseURL string) []string {
	var urls []string
	lines := splitLines(m3u8)
	for _, line := range lines {
		if len(line) > 0 && line[0] != '#' {
			// This is a TS segment URL
			tsURL := resolveURL(baseURL, line)
			urls = append(urls, tsURL)
		}
	}
	return urls
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			if line != "" {
				lines = append(lines, line)
			}
			start = i + 1
		}
	}
	if start < len(s) {
		line := s[start:]
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func resolveURL(baseURL, ref string) string {
	if len(ref) > 7 && (ref[:7] == "http://" || ref[:7] == "https:/") {
		return ref
	}
	// Simple resolution: strip filename from base and append ref
	for i := len(baseURL) - 1; i >= 0; i-- {
		if baseURL[i] == '/' {
			return baseURL[:i+1] + ref
		}
	}
	return ref
}

// HandleHLS serves HLS content over HTTP (fallback mode)
func (sr *StreamRelay) HandleHLS(c interface{ WriteHeader(int); Write([]byte) int; Header() http.Header }, path string) {
	sr.segMu.RLock()
	defer sr.segMu.RUnlock()

	// If requesting m3u8, serve the encrypted m3u8
	if path == "stream.m3u8" || path == "" {
		for i := len(sr.segments) - 1; i >= 0; i-- {
			if sr.segments[i].Type == "m3u8" {
				// In HLS mode, we serve the raw m3u8 (encryption handled by EXT-X-KEY)
				// For now, return the encrypted version
				c.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
				c.Header().Set("Cache-Control", "no-cache")
				c.WriteHeader(200)
				c.Write(sr.segments[i].Data)
				return
			}
		}
		c.WriteHeader(404)
		return
	}

	// TS segment request
	for i := len(sr.segments) - 1; i >= 0; i-- {
		seg := sr.segments[i]
		if seg.Type == "ts" {
			c.Header().Set("Content-Type", "video/mp2t")
			c.WriteHeader(200)
			c.Write(seg.Data)
			return
		}
	}
	c.WriteHeader(404)
}

// HandleWebSocket handles a WebSocket client connection
func (sr *StreamRelay) HandleWebSocket(conn *websocket.Conn) {
	clientID := sr.AddClient(conn)
	defer sr.RemoveClient(clientID)

	// Read loop (client sends heartbeats / requests)
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			return
		}
	}
}

// MeasureRTT measures round-trip time to the origin
func (sr *StreamRelay) MeasureRTT() int {
	start := time.Now()
	resp, err := http.Head(sr.originURL)
	if err != nil {
		return 9999
	}
	resp.Body.Close()
	return int(time.Since(start).Milliseconds())
}

// GetLocalIP returns the first non-loopback IP
func GetLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return "127.0.0.1"
}

// FormatBandwidth formats bytes/sec to human readable
func FormatBandwidth(bps int64) string {
	if bps < 1024 {
		return fmt.Sprintf("%d B/s", bps)
	}
	if bps < 1024*1024 {
		return fmt.Sprintf("%.1f KB/s", float64(bps)/1024)
	}
	return fmt.Sprintf("%.1f MB/s", float64(bps)/(1024*1024))
}
