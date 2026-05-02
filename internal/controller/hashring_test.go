package controller

import (
	"fmt"
	"math"
	"testing"

	"github.com/user/live-cdn/internal/common"
)

func TestConsistentHashBasicDispatch(t *testing.T) {
	store := NewMemoryStore()
	// 注册并审批 5 个节点
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("node-%d", i)
		store.RegisterNode(&common.RegisterRequest{
			NodeID: id, PublicIP: "1.1.1.1", Port: 9090,
			Region: "华东", ISP: "电信", Token: "t",
		})
		store.SetNodeStatus(id, common.NodeStatusOnline)
		store.UpdateHeartbeat(&common.HeartbeatRequest{
			NodeID: id, BWUsed: 2_000_000, BWLimit: 10_000_000, RTT: 10, LossRate: 0.01,
		})
	}

	chs := NewConsistentHashScheduler(store)
	result, err := chs.Dispatch("stream-1", "1.2.3.4", "华东", "电信")
	if err != nil {
		t.Fatalf("Dispatch failed: %v", err)
	}
	if len(result.Nodes) == 0 {
		t.Fatal("expected at least 1 node")
	}
	// 主节点优先级 0
	if result.Nodes[0].Priority != 0 {
		t.Errorf("expected priority 0, got %d", result.Nodes[0].Priority)
	}
}

func TestConsistentHashStickyMapping(t *testing.T) {
	store := NewMemoryStore()
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("node-%d", i)
		store.RegisterNode(&common.RegisterRequest{
			NodeID: id, PublicIP: "1.1.1.1", Port: 9090,
			Region: "华东", ISP: "电信", Token: "t",
		})
		store.SetNodeStatus(id, common.NodeStatusOnline)
		store.UpdateHeartbeat(&common.HeartbeatRequest{
			NodeID: id, BWUsed: 2_000_000, BWLimit: 10_000_000, RTT: 10, LossRate: 0.01,
		})
	}

	chs := NewConsistentHashScheduler(store)

	// 同一个 clientIP 应该总是映射到同一个主节点
	var primaryNode string
	for i := 0; i < 10; i++ {
		result, err := chs.Dispatch("stream-1", "5.6.7.8", "华东", "电信")
		if err != nil {
			t.Fatalf("Dispatch %d failed: %v", i, err)
		}
		if i == 0 {
			primaryNode = result.Nodes[0].Domain
		} else if result.Nodes[0].Domain != primaryNode {
			t.Errorf("same clientIP mapped to different primary node: %s vs %s", primaryNode, result.Nodes[0].Domain)
		}
	}
}

func TestConsistentHashUniformity(t *testing.T) {
	store := NewMemoryStore()
	nodeCount := 10
	nodeIDs := make([]string, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		id := fmt.Sprintf("node-%d", i)
		nodeIDs = append(nodeIDs, id)
		store.RegisterNode(&common.RegisterRequest{
			NodeID: id, PublicIP: fmt.Sprintf("10.0.0.%d", i), Port: 9090,
			Region: "华东", ISP: "电信", Token: "t",
			Domain: id,
		})
		store.SetNodeStatus(id, common.NodeStatusOnline)
		store.UpdateHeartbeat(&common.HeartbeatRequest{
			NodeID: id, BWUsed: 2_000_000, BWLimit: 10_000_000, RTT: 10, LossRate: 0.01,
		})
	}

	chs := NewConsistentHashScheduler(store)

	// 生成 1000 个不同的客户端 IP (确保每个 IP 唯一)
	nodeHits := make(map[string]int)
	totalClients := 1000
	for i := 0; i < totalClients; i++ {
		clientIP := fmt.Sprintf("192.%d.%d.%d", (i>>16)&0xFF, (i>>8)&0xFF, i&0xFF)
		result, err := chs.Dispatch("stream-1", clientIP, "华东", "电信")
		if err != nil {
			continue
		}
		if len(result.Nodes) > 0 {
			nodeHits[result.Nodes[0].Domain]++
		}
	}

	// 基本检查: 所有节点都应该被分配到
	t.Logf("nodeHits map: %v", nodeHits)
	if len(nodeHits) != nodeCount {
		t.Errorf("expected %d nodes to receive hits, got %d", nodeCount, len(nodeHits))
	}

	// 验证均匀性: 每个节点应该分配到约 totalClients/nodeCount 个客户端
	expected := float64(totalClients) / float64(nodeCount)

	// 计算标准差和变异系数
	var sum, sumSq float64
	for _, id := range nodeIDs {
		hits := float64(nodeHits[id])
		sum += hits
		sumSq += hits * hits
	}
	mean := sum / float64(nodeCount)
	variance := sumSq/float64(nodeCount) - mean*mean
	stddev := 0.0
	if variance > 0 {
		stddev = math.Sqrt(variance)
	}
	cv := stddev / mean * 100

	t.Logf("Uniformity: mean=%.1f, stddev=%.2f, CV=%.1f%%", mean, stddev, cv)

	// CV < 30% 认为均匀性可接受 (虚拟节点下应该很容易达到)
	if cv > 30 {
		t.Errorf("coefficient of variation too high: %.1f%% (expected < 30%%)", cv)
	}

	// 每个节点至少收到期望的 30%, 最多 200%
	minExpected := expected * 0.3
	maxExpected := expected * 2.0
	for _, id := range nodeIDs {
		hits := float64(nodeHits[id])
		if hits < minExpected {
			t.Errorf("node %s received %.0f hits, below minimum %.0f", id, hits, minExpected)
		}
		if hits > maxExpected {
			t.Errorf("node %s received %.0f hits, above maximum %.0f", id, hits, maxExpected)
		}
	}
}

func TestConsistentHashScaling(t *testing.T) {
	store := NewMemoryStore()
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("node-%d", i)
		store.RegisterNode(&common.RegisterRequest{
			NodeID: id, PublicIP: "1.1.1.1", Port: 9090,
			Region: "华东", ISP: "电信", Token: "t",
		})
		store.SetNodeStatus(id, common.NodeStatusOnline)
		store.UpdateHeartbeat(&common.HeartbeatRequest{
			NodeID: id, BWUsed: 2_000_000, BWLimit: 10_000_000, RTT: 10, LossRate: 0.01,
		})
	}

	chs := NewConsistentHashScheduler(store)

	// 记录 100 个客户端的原始分配
	originalAssignments := make(map[string]string) // clientIP -> nodeRegion+ISP
	for i := 0; i < 100; i++ {
		clientIP := fmt.Sprintf("10.0.%d.%d", i/256, i%256)
		result, _ := chs.Dispatch("stream-1", clientIP, "华东", "电信")
		if len(result.Nodes) > 0 {
			originalAssignments[clientIP] = result.Nodes[0].Region + "/" + result.Nodes[0].ISP
		}
	}

	// 新增 1 个节点
	store.RegisterNode(&common.RegisterRequest{
		NodeID: "node-new", PublicIP: "10.0.0.99", Port: 9090,
		Region: "华南", ISP: "移动", Token: "t",
	})
	store.SetNodeStatus("node-new", common.NodeStatusOnline)
	store.UpdateHeartbeat(&common.HeartbeatRequest{
		NodeID: "node-new", BWUsed: 1_000_000, BWLimit: 10_000_000, RTT: 10, LossRate: 0.01,
	})

	// 重新分配, 统计有多少客户端换了节点
	changed := 0
	for clientIP, originalKey := range originalAssignments {
		result, _ := chs.Dispatch("stream-1", clientIP, "华东", "电信")
		if len(result.Nodes) > 0 {
			newKey := result.Nodes[0].Region + "/" + result.Nodes[0].ISP
			if newKey != originalKey {
				changed++
			}
		}
	}

	// 扩容 1 个节点 (5→6), 理论上只有 1/6 ≈ 17% 的客户端需要重新分配
	// 允许到 30%
	changeRate := float64(changed) / float64(len(originalAssignments))
	if changeRate > 0.35 {
		t.Errorf("scaling: %.1f%% clients reassigned (expected ~17%%)", changeRate*100)
	}
}

func TestConsistentHashFallback(t *testing.T) {
	store := NewMemoryStore()
	chs := NewConsistentHashScheduler(store)

	// 没有节点 → 应该 fallback 返回错误
	_, err := chs.Dispatch("stream-1", "1.2.3.4", "华东", "电信")
	if err == nil {
		t.Error("expected error with no nodes")
	}
}
