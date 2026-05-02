package controller

import (
	"testing"

	"github.com/user/live-cdn/internal/common"
)

func TestComputeTier(t *testing.T) {
	tests := []struct {
		name     string
		node     *common.NodeInfo
		expected int
	}{
		{
			"Tier1: 优选CDN (cascade_enabled + 直连Origin)",
			&common.NodeInfo{CascadeEnabled: true, CascadeUpstream: false},
			1,
		},
		{
			"Tier2: 边缘CDN (不参与级联)",
			&common.NodeInfo{CascadeEnabled: false, CascadeUpstream: false},
			2,
		},
		{
			"Tier2: 边缘CDN (从Tier1拉流)",
			&common.NodeInfo{CascadeEnabled: false, CascadeUpstream: true, CascadeDepth: 1},
			2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := computeTier(tt.node)
			if result != tt.expected {
				t.Errorf("computeTier() = %d, want %d", result, tt.expected)
			}
		})
	}
}

func TestMeshNodeScoreV2_RegionBonus(t *testing.T) {
	node := &common.NodeInfo{
		Region:  "华东",
		ISP:     "电信",
		BWUsed:  5_000_000,
		BWLimit: 10_000_000,
	}

	score1 := meshNodeScoreV2(node, "华东", "电信")  // 同区同ISP
	score2 := meshNodeScoreV2(node, "华东", "联通")  // 同区不同ISP
	score3 := meshNodeScoreV2(node, "华北", "移动")  // 跨区

	if score1 <= score2 {
		t.Errorf("同区同ISP (%.2f) 应高于同区不同ISP (%.2f)", score1, score2)
	}
	if score2 <= score3 {
		t.Errorf("同区 (%.2f) 应高于跨区 (%.2f)", score2, score3)
	}
}

func TestMeshNodeScoreV2_CapacityBonus(t *testing.T) {
	light := &common.NodeInfo{
		Region:  "华北", ISP: "联通",
		BWUsed: 1_000_000, BWLimit: 10_000_000,
	}
	heavy := &common.NodeInfo{
		Region:  "华北", ISP: "联通",
		BWUsed: 9_000_000, BWLimit: 10_000_000,
	}

	scoreLight := meshNodeScoreV2(light, "华北", "联通")
	scoreHeavy := meshNodeScoreV2(heavy, "华北", "联通")

	if scoreLight <= scoreHeavy {
		t.Errorf("低负载 (%.2f) 应高于高负载 (%.2f)", scoreLight, scoreHeavy)
	}
}

func TestBuildMeshMapResponse(t *testing.T) {
	store := NewMemoryStore()
	s := NewServer(&Config{
		RegToken:   "test",
		AdminToken: "admin",
		OriginAddr: "http://origin:8080",
	})
	s.store = store

	// 注册: 请求者 (Tier2, 不做上游)
	store.RegisterNode(&common.RegisterRequest{
		NodeID: "requester", PublicIP: "1.1.1.1", Port: 9090,
		Region: "华东", ISP: "电信", Token: "test",
		CascadeEnabled: false, CascadeUpstream: false,
	})
	// 注册: 上游1 (Tier1, cascade_enabled)
	store.RegisterNode(&common.RegisterRequest{
		NodeID: "upstream-1", PublicIP: "2.2.2.2", Port: 9090,
		Region: "华东", ISP: "电信", Token: "test",
		CascadeEnabled: true, CascadeUpstream: false,
	})
	// 注册: 上游2 (Tier1, 但不同区)
	store.RegisterNode(&common.RegisterRequest{
		NodeID: "upstream-2", PublicIP: "3.3.3.3", Port: 9090,
		Region: "华北", ISP: "联通", Token: "test",
		CascadeEnabled: true, CascadeUpstream: false,
	})

	store.SetNodeStatus("requester", common.NodeStatusOnline)
	store.SetNodeStatus("upstream-1", common.NodeStatusOnline)
	store.SetNodeStatus("upstream-2", common.NodeStatusOnline)

	resp := s.buildMeshMapResponse("requester")
	if resp == nil {
		t.Fatal("response should not be nil")
	}

	// 不应包含自己
	for _, c := range resp.CandidateUpstreams {
		if c.NodeID == "requester" {
			t.Error("candidates should not include self")
		}
	}

	// 应有候选上游
	if len(resp.CandidateUpstreams) == 0 {
		t.Error("should have candidate upstreams")
	}

	// Policy 应有合理默认值
	if resp.Policy.MaxDepth != 2 {
		t.Errorf("MaxDepth should be 2, got %d", resp.Policy.MaxDepth)
	}
	if resp.Policy.SwitchCooldownSec != 30 {
		t.Errorf("SwitchCooldownSec should be 30, got %d", resp.Policy.SwitchCooldownSec)
	}
}

func TestBuildMeshMapResponse_TierEnforcement(t *testing.T) {
	store := NewMemoryStore()
	s := NewServer(&Config{
		RegToken:   "test",
		AdminToken: "admin",
		OriginAddr: "http://origin:8080",
	})
	s.store = store

	// 请求者也是 Tier1 (cascade_enabled, 直连Origin)
	store.RegisterNode(&common.RegisterRequest{
		NodeID: "tier1-node", PublicIP: "1.1.1.1", Port: 9090,
		Region: "华东", ISP: "电信", Token: "test",
		CascadeEnabled: true, CascadeUpstream: false,
	})
	// 另一个 Tier1 节点
	store.RegisterNode(&common.RegisterRequest{
		NodeID: "tier1-other", PublicIP: "2.2.2.2", Port: 9090,
		Region: "华东", ISP: "电信", Token: "test",
		CascadeEnabled: true, CascadeUpstream: false,
	})

	store.SetNodeStatus("tier1-node", common.NodeStatusOnline)
	store.SetNodeStatus("tier1-other", common.NodeStatusOnline)

	resp := s.buildMeshMapResponse("tier1-node")

	// Tier1 不应从同层 Tier1 拉流 (tier < selfTier 规则)
	for _, c := range resp.CandidateUpstreams {
		if c.Tier >= resp.SelfTier {
			t.Errorf("candidate tier %d should be < self tier %d", c.Tier, resp.SelfTier)
		}
	}
}
