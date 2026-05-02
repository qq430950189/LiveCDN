package controller

import (
	"testing"
	"time"

	"github.com/user/live-cdn/internal/common"
)

// helper: 注册并审批节点
func registerAndApprove(store *MemoryStore, id, region, isp string) {
	store.RegisterNode(&common.RegisterRequest{
		NodeID: id, PublicIP: "1.1.1.1", Port: 9090,
		Region: region, ISP: isp, Token: "t",
	})
	store.SetNodeStatus(id, common.NodeStatusOnline)
}

func TestRegisterNode(t *testing.T) {
	store := NewMemoryStore()

	req := &common.RegisterRequest{
		NodeID:   "node-1",
		PublicIP: "1.2.3.4",
		Port:     9090,
		Region:   "华东",
		ISP:      "电信",
		BWLimit:  10485760,
		Token:    "test-token",
	}

	err := store.RegisterNode(req)
	if err != nil {
		t.Fatalf("RegisterNode failed: %v", err)
	}

	node, ok := store.GetNode("node-1")
	if !ok {
		t.Fatal("node not found after register")
	}
	if node.Region != "华东" || node.ISP != "电信" {
		t.Errorf("expected 华东/电信, got %s/%s", node.Region, node.ISP)
	}
	// 新节点默认 pending
	if node.Status != common.NodeStatusPending {
		t.Errorf("expected pending, got %s", node.Status)
	}

	// 审批后变 online
	store.SetNodeStatus("node-1", common.NodeStatusOnline)
	node, _ = store.GetNode("node-1")
	if node.Status != common.NodeStatusOnline {
		t.Errorf("expected online after approval, got %s", node.Status)
	}
}

func TestNodeReconnect(t *testing.T) {
	store := NewMemoryStore()

	// 先注册并审批
	registerAndApprove(store, "node-1", "华东", "电信")

	// 模拟重连: 再次注册
	store.RegisterNode(&common.RegisterRequest{
		NodeID: "node-1", PublicIP: "2.2.2.2", Port: 9091,
		Region: "华东", ISP: "电信", Token: "t",
	})

	// 重连节点应保持 online 状态
	node, _ := store.GetNode("node-1")
	if node.Status != common.NodeStatusOnline {
		t.Errorf("reconnecting node should stay online, got %s", node.Status)
	}
	if node.PublicIP != "2.2.2.2" {
		t.Errorf("IP should be updated on reconnect, got %s", node.PublicIP)
	}
}

func TestHeartbeat(t *testing.T) {
	store := NewMemoryStore()
	registerAndApprove(store, "node-1", "华东", "电信")

	hb := &common.HeartbeatRequest{
		NodeID:      "node-1",
		BWUsed:      5000000,
		BWLimit:     10485760,
		OnlineUsers: 42,
		RTT:         15,
		LossRate:    0.01,
	}

	err := store.UpdateHeartbeat(hb)
	if err != nil {
		t.Fatalf("UpdateHeartbeat failed: %v", err)
	}

	node, _ := store.GetNode("node-1")
	if node.OnlineUsers != 42 {
		t.Errorf("expected 42 users, got %d", node.OnlineUsers)
	}
	if node.RTT != 15 {
		t.Errorf("expected RTT 15, got %d", node.RTT)
	}
}

func TestHeartbeatUnknownNode(t *testing.T) {
	store := NewMemoryStore()
	hb := &common.HeartbeatRequest{NodeID: "ghost"}
	err := store.UpdateHeartbeat(hb)
	if err == nil {
		t.Error("expected error for unknown node")
	}
}

func TestGetHealthyNodes(t *testing.T) {
	store := NewMemoryStore()

	// 注册3个节点并审批
	for _, cfg := range []struct {
		id   string
		bwU  int64
		bwL  int64
		loss float64
	}{
		{"healthy", 3_000_000, 10_000_000, 0.01},
		{"busy", 8_000_000, 10_000_000, 0.02},
		{"lossy", 2_000_000, 10_000_000, 0.10},
	} {
		store.RegisterNode(&common.RegisterRequest{
			NodeID: cfg.id, PublicIP: "1.1.1.1", Port: 9090,
			Region: "华东", ISP: "电信", Token: "t",
		})
		store.SetNodeStatus(cfg.id, common.NodeStatusOnline)
		store.UpdateHeartbeat(&common.HeartbeatRequest{
			NodeID:   cfg.id,
			BWUsed:   cfg.bwU,
			BWLimit:  cfg.bwL,
			RTT:      10,
			LossRate: cfg.loss,
		})
	}

	healthy := store.GetHealthyNodes()
	if len(healthy) != 1 {
		t.Errorf("expected 1 healthy node, got %d", len(healthy))
	}
	if len(healthy) > 0 && healthy[0].NodeID != "healthy" {
		t.Errorf("expected healthy node, got %s", healthy[0].NodeID)
	}
}

func TestGetNodesByRegion(t *testing.T) {
	store := NewMemoryStore()

	regions := []struct {
		id, region, isp string
	}{
		{"n1", "华东", "电信"},
		{"n2", "华东", "移动"},
		{"n3", "华南", "电信"},
	}
	for _, r := range regions {
		registerAndApprove(store, r.id, r.region, r.isp)
	}

	// 同区同运营商
	nodes := store.GetNodesByRegion("华东", "电信")
	if len(nodes) != 1 || nodes[0].NodeID != "n1" {
		t.Errorf("expected n1, got %v", nodes)
	}

	// 同区不同运营商
	nodes = store.GetNodesByRegion("华东", "联通")
	if len(nodes) != 0 {
		t.Errorf("expected 0, got %d", len(nodes))
	}
}

func TestRemoveStaleNodes(t *testing.T) {
	store := NewMemoryStore()

	store.RegisterNode(&common.RegisterRequest{
		NodeID: "old", PublicIP: "1.1.1.1", Port: 9090,
		Region: "华东", ISP: "电信", Token: "t",
	})
	store.RegisterNode(&common.RegisterRequest{
		NodeID: "new", PublicIP: "2.2.2.2", Port: 9090,
		Region: "华东", ISP: "电信", Token: "t",
	})

	// 模拟 old 节点心跳超时
	store.mu.Lock()
	store.nodes["old"].LastHB = time.Now().Add(-5 * time.Minute)
	store.mu.Unlock()

	removed := store.RemoveStaleNodes(2 * time.Minute)
	if removed != 1 {
		t.Errorf("expected 1 removed, got %d", removed)
	}

	_, ok := store.GetNode("old")
	if ok {
		t.Error("old node should be removed")
	}
	_, ok = store.GetNode("new")
	if !ok {
		t.Error("new node should still exist")
	}
}

func TestStreamLifecycle(t *testing.T) {
	store := NewMemoryStore()

	stream := &common.StreamInfo{
		StreamKey:   "test-stream-1",
		Title:       "Test",
		IsLive:      true,
		CipherSuite: "chacha20-poly1305",
		CreatedAt:   time.Now(),
		LatencyMode: common.LatencyStandard,
	}
	store.CreateStream(stream)

	s, ok := store.GetStream("test-stream-1")
	if !ok || !s.IsLive {
		t.Error("stream should be live")
	}

	store.EndStream("test-stream-1")
	s, _ = store.GetStream("test-stream-1")
	if s.IsLive {
		t.Error("stream should be ended")
	}
}

func TestTokenManagement(t *testing.T) {
	store := NewMemoryStore()
	store.SetToken("viewer-token-123", "stream-abc")

	sk, ok := store.ValidateToken("viewer-token-123")
	if !ok || sk != "stream-abc" {
		t.Errorf("expected stream-abc, got %s", sk)
	}

	_, ok = store.ValidateToken("invalid-token")
	if ok {
		t.Error("invalid token should fail")
	}
}

func TestSessionManagement(t *testing.T) {
	store := NewMemoryStore()

	session := &common.SessionInfo{
		SessionID:  "sess-1",
		StreamKey:  "stream-1",
		ClientIP:   "10.0.0.1",
		StartTime:  time.Now(),
		LastActive: time.Now(),
	}
	store.CreateSession(session)

	s, ok := store.GetSession("sess-1")
	if !ok || s.StreamKey != "stream-1" {
		t.Error("session not found or wrong data")
	}

	store.RemoveSession("sess-1")
	_, ok = store.GetSession("sess-1")
	if ok {
		t.Error("session should be removed")
	}
}

func TestComplaintCooling(t *testing.T) {
	store := NewMemoryStore()
	registerAndApprove(store, "node-1", "华东", "电信")

	// 2 次投诉不应该冷却
	store.RecordComplaint("node-1")
	store.RecordComplaint("node-1")
	healthy := store.GetHealthyNodes()
	if len(healthy) != 1 {
		t.Errorf("expected 1 healthy after 2 complaints, got %d", len(healthy))
	}

	// 第 3 次投诉触发冷却
	store.RecordComplaint("node-1")
	healthy = store.GetHealthyNodes()
	if len(healthy) != 0 {
		t.Errorf("expected 0 healthy after 3 complaints (cooling), got %d", len(healthy))
	}
}

func TestGetLiveStreams(t *testing.T) {
	store := NewMemoryStore()

	store.CreateStream(&common.StreamInfo{StreamKey: "s1", IsLive: true})
	store.CreateStream(&common.StreamInfo{StreamKey: "s2", IsLive: true})
	store.CreateStream(&common.StreamInfo{StreamKey: "s3", IsLive: false})

	live := store.GetLiveStreams()
	if len(live) != 2 {
		t.Errorf("expected 2 live streams, got %d", len(live))
	}
}

func TestPendingNodeNotHealthy(t *testing.T) {
	store := NewMemoryStore()

	// 注册但未审批
	store.RegisterNode(&common.RegisterRequest{
		NodeID: "pending-node", PublicIP: "1.1.1.1", Port: 9090,
		Region: "华东", ISP: "电信", Token: "t",
	})

	// pending 节点不应出现在 healthy 列表
	healthy := store.GetHealthyNodes()
	if len(healthy) != 0 {
		t.Errorf("pending node should not be healthy, got %d", len(healthy))
	}

	// 审批后
	store.SetNodeStatus("pending-node", common.NodeStatusOnline)
	healthy = store.GetHealthyNodes()
	if len(healthy) != 1 {
		t.Errorf("approved node should be healthy, got %d", len(healthy))
	}
}
