package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/user/live-cdn/internal/common"
	"github.com/user/live-cdn/internal/crypto"
)

func TestApplyConfigDefaultsNormalizesCompatibilityValues(t *testing.T) {
	cfg := &Config{
		OriginAddr:  "http://origin:8080",
		CipherSuite: "chacha20",
	}

	applyConfigDefaults(cfg)

	if cfg.ListenAddr != ":8080" {
		t.Fatalf("ListenAddr = %q, want :8080", cfg.ListenAddr)
	}
	if cfg.RTMPOriginAddr != "origin:1935" {
		t.Fatalf("RTMPOriginAddr = %q, want origin:1935", cfg.RTMPOriginAddr)
	}
	if cfg.CipherSuite != string(crypto.CipherChaCha20) {
		t.Fatalf("CipherSuite = %q, want %q", cfg.CipherSuite, crypto.CipherChaCha20)
	}
}

func TestBroadcastStartReturnsProtocolCorrectURLs(t *testing.T) {
	s := NewServer(&Config{
		ListenAddr:     ":0",
		OriginAddr:     "http://origin:8080/",
		RTMPOriginAddr: "rtmp://origin:1935/",
		AdminToken:     "admin-token",
		RegToken:       "reg-token",
		CipherSuite:    "chacha20", // legacy alias should remain accepted
	})

	body := bytes.NewBufferString(`{"title":"compat","access_token":"admin-token"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/broadcast/start", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var resp struct {
		StreamKey   string `json:"stream_key"`
		PushURL     string `json:"push_url"`
		HLSURL      string `json:"hls_url"`
		ViewerToken string `json:"viewer_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.StreamKey == "" || resp.ViewerToken == "" {
		t.Fatalf("expected stream key and viewer token, got %+v", resp)
	}
	if strings.Contains(resp.PushURL, "rtmp://http://") {
		t.Fatalf("push URL contains mixed schemes: %s", resp.PushURL)
	}
	if !strings.HasPrefix(resp.PushURL, "rtmp://origin:1935/live/") {
		t.Fatalf("push URL = %q, want rtmp origin", resp.PushURL)
	}
	if !strings.HasPrefix(resp.HLSURL, "http://origin:8080/live/") || !strings.HasSuffix(resp.HLSURL, "/stream.m3u8") {
		t.Fatalf("HLS URL = %q, want HTTP HLS playlist URL", resp.HLSURL)
	}
}

func TestAgentHeartbeatRequiresTokenAndUpdatesNode(t *testing.T) {
	s := NewServer(&Config{
		ListenAddr:  ":0",
		OriginAddr:  "http://origin:8080",
		AdminToken:  "admin-token",
		RegToken:    "reg-token",
		CipherSuite: "chacha20-poly1305",
	})

	registerBody := bytes.NewBufferString(`{"node_id":"edge-1","public_ip":"127.0.0.1","port":9090,"region":"test","isp":"test","bw_limit":1000,"domain":"localhost","protocol":"ws","ws_path":"/ws/live","token":"reg-token"}`)
	registerReq := httptest.NewRequest(http.MethodPost, "/api/agent/register", registerBody)
	registerReq.Header.Set("Content-Type", "application/json")
	registerResp := httptest.NewRecorder()
	s.router.ServeHTTP(registerResp, registerReq)
	if registerResp.Code != http.StatusOK {
		t.Fatalf("register status = %d, body = %s", registerResp.Code, registerResp.Body.String())
	}

	heartbeatBody := bytes.NewBufferString(`{"node_id":"edge-1","bw_used":10,"bw_limit":1000,"online_users":2,"rtt":20}`)
	unauthorized := httptest.NewRequest(http.MethodPost, "/api/agent/heartbeat", heartbeatBody)
	unauthorized.Header.Set("Content-Type", "application/json")
	unauthorizedResp := httptest.NewRecorder()
	s.router.ServeHTTP(unauthorizedResp, unauthorized)
	if unauthorizedResp.Code != http.StatusForbidden {
		t.Fatalf("unauthorized heartbeat status = %d, want %d", unauthorizedResp.Code, http.StatusForbidden)
	}

	authorizedBody := bytes.NewBufferString(`{"node_id":"edge-1","bw_used":10,"bw_limit":1000,"online_users":2,"rtt":20}`)
	req := httptest.NewRequest(http.MethodPost, "/api/agent/heartbeat", authorizedBody)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer reg-token")
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d, body = %s", w.Code, w.Body.String())
	}

	node, ok := s.store.GetNode("edge-1")
	if !ok {
		t.Fatal("expected registered node to exist")
	}
	if node.RTT != 20 || node.OnlineUsers != 2 {
		t.Fatalf("node heartbeat fields not updated: %+v", node)
	}
}

func TestAgentGetStreamInfoRequiresTokenAndReturnsLiveStream(t *testing.T) {
	s := NewServer(&Config{
		ListenAddr:  ":0",
		OriginAddr:  "http://origin:8080",
		AdminToken:  "admin-token",
		RegToken:    "reg-token",
		CipherSuite: "chacha20-poly1305",
	})

	stream := &common.StreamInfo{
		StreamKey:   "stream-1",
		Title:       "agent fetch",
		OriginURL:   "http://origin:8080/live/stream-1/stream.m3u8",
		EncryptKey:  "key",
		EncryptIV:   "iv",
		CipherSuite: "chacha20-poly1305",
		IsLive:      true,
		CreatedAt:   time.Now(),
	}
	s.store.CreateStream(stream)

	unauthorized := httptest.NewRequest(http.MethodGet, "/api/agent/streams/stream-1", nil)
	unauthorizedResp := httptest.NewRecorder()
	s.router.ServeHTTP(unauthorizedResp, unauthorized)
	if unauthorizedResp.Code != http.StatusForbidden {
		t.Fatalf("unauthorized status = %d, want %d", unauthorizedResp.Code, http.StatusForbidden)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/agent/streams/stream-1", nil)
	req.Header.Set("Authorization", "Bearer reg-token")
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var got common.StreamInfo
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.StreamKey != stream.StreamKey || got.EncryptKey != stream.EncryptKey || got.OriginURL != stream.OriginURL {
		t.Fatalf("unexpected stream info response: %+v", got)
	}
}

func TestPlayerQualityReportTouchesAndEndsSession(t *testing.T) {
	s := NewServer(&Config{ListenAddr: ":0", OriginAddr: "http://origin:8080", RegToken: "reg", AdminToken: "admin"})
	sessionID := "session-1"
	oldActive := time.Now().Add(-time.Minute)
	s.store.CreateSession(&common.SessionInfo{
		SessionID:  sessionID,
		StreamKey:  "stream-1",
		NodeID:     "node-1",
		ClientIP:   "127.0.0.1",
		StartTime:  oldActive,
		LastActive: oldActive,
	})

	reportBody := bytes.NewBufferString(`{"session_id":"session-1","node_id":"node-1","stall_rate":0,"rtt":1,"e2e_latency":100}`)
	req := httptest.NewRequest(http.MethodPost, "/api/player/report", reportBody)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("quality report status = %d, body = %s", w.Code, w.Body.String())
	}

	session, ok := s.store.GetSession(sessionID)
	if !ok {
		t.Fatal("expected session to remain after quality report")
	}
	if !session.LastActive.After(oldActive) {
		t.Fatalf("expected LastActive to be touched, got %s <= %s", session.LastActive, oldActive)
	}

	endBody := bytes.NewBufferString(`{"session_id":"session-1"}`)
	endReq := httptest.NewRequest(http.MethodPost, "/api/player/session/end", endBody)
	endReq.Header.Set("Content-Type", "application/json")
	endResp := httptest.NewRecorder()
	s.router.ServeHTTP(endResp, endReq)
	if endResp.Code != http.StatusOK {
		t.Fatalf("end session status = %d, body = %s", endResp.Code, endResp.Body.String())
	}
	if _, ok := s.store.GetSession(sessionID); ok {
		t.Fatal("expected session to be removed")
	}
}

func TestPlayerQualityReportRejectsUnknownSession(t *testing.T) {
	s := NewServer(&Config{ListenAddr: ":0", OriginAddr: "http://origin:8080", RegToken: "reg", AdminToken: "admin"})

	body := bytes.NewBufferString(`{"session_id":"missing","node_id":"node-1","stall_rate":0.2}`)
	req := httptest.NewRequest(http.MethodPost, "/api/player/report", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("quality report status = %d, want %d", w.Code, http.StatusNotFound)
	}
}
