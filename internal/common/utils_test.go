package common

import (
	"testing"
	"time"
)

func TestCalcLatencyScore(t *testing.T) {
	tests := []struct {
		rtt   int
		min   float64
		max   float64
	}{
		{0, 0.99, 1.0},    // 未测量
		{10, 0.99, 1.0},   // 极低延迟
		{30, 0.7, 1.0},    // 低延迟
		{80, 0.5, 0.95},   // 中延迟
		{200, 0.1, 0.7},   // 高延迟
		{500, 0.01, 0.15}, // 很高延迟
		{1000, 0.01, 0.1}, // 极高延迟
	}

	for _, tt := range tests {
		score := calcLatencyScore(tt.rtt)
		if score < tt.min || score > tt.max {
			t.Errorf("RTT=%d: score=%.3f, expected [%.3f, %.3f]", tt.rtt, score, tt.min, tt.max)
		}
	}

	// 延迟越低分越高
	if calcLatencyScore(10) < calcLatencyScore(100) {
		t.Error("lower RTT should score higher")
	}
}

func TestCalcLoadScore(t *testing.T) {
	// 空载
	s1 := calcLoadScore(0, 0, 10_000_000)
	if s1 < 0.9 {
		t.Errorf("empty node score too low: %.3f", s1)
	}

	// 半载
	s2 := calcLoadScore(50, 5_000_000, 10_000_000)
	if s2 >= s1 {
		t.Error("loaded node should score lower than empty")
	}

	// 高载
	s3 := calcLoadScore(200, 8_000_000, 10_000_000)
	if s3 >= s2 {
		t.Error("heavy node should score lower than moderate")
	}

	t.Logf("空载=%.3f 半载=%.3f 高载=%.3f", s1, s2, s3)
}

func TestCalcQualityScore(t *testing.T) {
	tests := []struct {
		loss  float64
		min   float64
		max   float64
	}{
		{0, 0.99, 1.0},    // 无丢包
		{0.01, 0.7, 0.95}, // 1% 丢包
		{0.03, 0.2, 0.6},   // 3% 丢包
		{0.05, -0.01, 0.01}, // 5% 丢包 → 趋近0 (应该被硬阈值剔除)
	}

	for _, tt := range tests {
		score := calcQualityScore(tt.loss)
		if score < tt.min || score > tt.max {
			t.Errorf("loss=%.2f: score=%.3f, expected [%.3f, %.3f]", tt.loss, score, tt.min, tt.max)
		}
	}
}

func TestNodeScoreMetricDriven(t *testing.T) {
	// 低延迟 + 低负载 + 低丢包 = 高分
	good := &NodeInfo{
		BWUsed: 1_000_000, BWLimit: 10_000_000,
		RTT: 10, LossRate: 0.001, OnlineUsers: 5,
	}

	// 高延迟 + 高负载 + 高丢包 = 低分
	bad := &NodeInfo{
		BWUsed: 8_000_000, BWLimit: 10_000_000,
		RTT: 200, LossRate: 0.03, OnlineUsers: 100,
	}

	goodScore := NodeScore(good)
	badScore := NodeScore(bad)

	if goodScore <= badScore {
		t.Errorf("good node (%.3f) should score higher than bad node (%.3f)", goodScore, badScore)
	}

	t.Logf("好节点=%.3f 差节点=%.3f", goodScore, badScore)
}

func TestNodeScoreRegionBonus(t *testing.T) {
	node := &NodeInfo{
		Region: "华东", ISP: "电信",
		BWUsed: 2_000_000, BWLimit: 10_000_000,
		RTT: 30, LossRate: 0.01, OnlineUsers: 20,
	}

	base := NodeScore(node)
	sameRegionSameISP := NodeScoreWithRegionBonus(node, "华东", "电信")
	sameRegionDiffISP := NodeScoreWithRegionBonus(node, "华东", "移动")
	diffRegion := NodeScoreWithRegionBonus(node, "华南", "电信")

	if sameRegionSameISP <= base {
		t.Error("same region+ISP should get bonus")
	}
	if sameRegionDiffISP <= base {
		t.Error("same region should get bonus")
	}
	if diffRegion != base {
		t.Error("different region should get no bonus")
	}

	t.Logf("基础=%.3f 同区同运营商=%.3f 同区不同运营商=%.3f 不同区=%.3f",
		base, sameRegionSameISP, sameRegionDiffISP, diffRegion)

	// 验证: 加分不能让差节点超过好节点
	goodDiffRegion := &NodeInfo{
		Region: "华南", ISP: "电信",
		BWUsed: 1_000_000, BWLimit: 10_000_000,
		RTT: 10, LossRate: 0.001, OnlineUsers: 5,
	}
	badSameRegion := &NodeInfo{
		Region: "华东", ISP: "电信",
		BWUsed: 7_000_000, BWLimit: 10_000_000,
		RTT: 150, LossRate: 0.03, OnlineUsers: 80,
	}

	goodWithBonus := NodeScoreWithRegionBonus(goodDiffRegion, "华东", "电信")
	badWithBonus := NodeScoreWithRegionBonus(badSameRegion, "华东", "电信")

	t.Logf("好节点(不同区+加分)=%.3f 差节点(同区+加分)=%.3f", goodWithBonus, badWithBonus)

	// 加分后好节点仍然应该高于差节点
	// 因为好节点基础分远高于差节点，+20% 不足以弥补
	if goodWithBonus <= badWithBonus {
		t.Error("region bonus should not override metric quality")
	}
}

func TestIsNodeHealthy(t *testing.T) {
	tests := []struct {
		name   string
		node   *NodeInfo
		healthy bool
	}{
		{"online", &NodeInfo{Status: NodeStatusOnline, LastHB: now(), BWLimit: 10, BWUsed: 1, LossRate: 0.01}, true},
		{"offline", &NodeInfo{Status: NodeStatusOffline, LastHB: now()}, false},
		{"cooling", &NodeInfo{Status: NodeStatusCooling, LastHB: now()}, false},
		{"heartbeat timeout", &NodeInfo{Status: NodeStatusOnline, LastHB: ago(20 * time.Second)}, false},
		{"bandwidth overload", &NodeInfo{Status: NodeStatusOnline, LastHB: now(), BWLimit: 10, BWUsed: 8}, false},
		{"high loss", &NodeInfo{Status: NodeStatusOnline, LastHB: now(), LossRate: 0.10}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if IsNodeHealthy(tt.node) != tt.healthy {
				t.Errorf("expected healthy=%v", tt.healthy)
			}
		})
	}
}

// helpers
func now() time.Time { return time.Now() }
func ago(d time.Duration) time.Time { return time.Now().Add(-d) }
