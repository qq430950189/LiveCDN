package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/user/live-cdn/internal/common"
)

func TestFetchStreamInfoQueriesController(t *testing.T) {
	const streamKey = "stream-with/slash"
	const token = "reg-token"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/agent/streams/stream-with%2Fslash" {
			t.Fatalf("path = %q, want escaped stream info path", r.URL.EscapedPath())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
		_ = json.NewEncoder(w).Encode(common.StreamInfo{
			StreamKey:   streamKey,
			OriginURL:   "http://origin:8080/live/stream-with%2Fslash/stream.m3u8",
			EncryptKey:  "test-key",
			EncryptIV:   "test-iv",
			CipherSuite: "chacha20-poly1305",
			IsLive:      true,
		})
	}))
	defer ts.Close()

	a := NewAgent(&Config{ControllerURL: ts.URL, RegToken: token})
	info, err := a.fetchStreamInfo(streamKey)
	if err != nil {
		t.Fatalf("fetchStreamInfo returned error: %v", err)
	}
	if info.StreamKey != streamKey || info.EncryptKey != "test-key" || info.EncryptIV != "test-iv" {
		t.Fatalf("unexpected stream info: %+v", info)
	}
}

func TestFetchStreamInfoPropagatesControllerStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "missing", http.StatusNotFound)
	}))
	defer ts.Close()

	a := NewAgent(&Config{ControllerURL: ts.URL, RegToken: "reg-token"})
	if _, err := a.fetchStreamInfo("missing"); err == nil {
		t.Fatal("expected fetchStreamInfo to return an error for a non-200 controller response")
	}
}

func TestControllerAPIURLTrimsTrailingSlash(t *testing.T) {
	a := NewAgent(&Config{ControllerURL: "http://controller:8080/"})
	got, err := a.controllerAPIURL("api/agent/heartbeat")
	if err != nil {
		t.Fatalf("controllerAPIURL returned error: %v", err)
	}
	want := "http://controller:8080/api/agent/heartbeat"
	if got != want {
		t.Fatalf("controllerAPIURL = %q, want %q", got, want)
	}
}

func TestSendHeartbeatUsesControllerEndpointAndReturnsStatus(t *testing.T) {
	const nodeID = "edge-1"
	const token = "reg-token"

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/heartbeat" {
			t.Fatalf("path = %q, want heartbeat endpoint", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
		var hb common.HeartbeatRequest
		if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
			t.Fatalf("decode heartbeat: %v", err)
		}
		if hb.NodeID != nodeID {
			t.Fatalf("heartbeat node_id = %q, want %q", hb.NodeID, nodeID)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	a := NewAgent(&Config{ControllerURL: ts.URL + "/", RegToken: token})
	status, err := a.sendHeartbeat(&common.HeartbeatRequest{NodeID: nodeID})
	if err != nil {
		t.Fatalf("sendHeartbeat returned error: %v", err)
	}
	if status != http.StatusNotFound {
		t.Fatalf("sendHeartbeat status = %d, want %d", status, http.StatusNotFound)
	}
}

func TestRegisterWithControllerUsesTrimmedEndpoint(t *testing.T) {
	const token = "reg-token"
	seenRegister := false

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/register" {
			t.Fatalf("path = %q, want register endpoint", r.URL.Path)
		}
		var reg common.RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
			t.Fatalf("decode register request: %v", err)
		}
		if reg.NodeID != "edge-1" || reg.Token != token {
			t.Fatalf("unexpected register request: %+v", reg)
		}
		seenRegister = true
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	a := NewAgent(&Config{
		NodeID:        "edge-1",
		PublicIP:      "127.0.0.1",
		Port:          9090,
		Region:        "test",
		ISP:           "test",
		ControllerURL: ts.URL + "/",
		RegToken:      token,
	})
	if err := a.RegisterWithController(); err != nil {
		t.Fatalf("RegisterWithController returned error: %v", err)
	}
	if !seenRegister {
		t.Fatal("expected register endpoint to be called")
	}
}
