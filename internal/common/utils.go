package common

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// GenerateStreamKey creates a random stream key
func GenerateStreamKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// GenerateToken creates a random access token
func GenerateToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// IsNodeHealthy checks if a node meets basic health criteria
// 所有硬阈值在这里判断，不满足的直接剔除，不进评分
// pending 状态的节点也视为不健康（待管理员审批）
func IsNodeHealthy(node *NodeInfo) bool {
	if node.Status == NodeStatusOffline || node.Status == NodeStatusCooling || node.Status == NodeStatusPending {
		return false
	}
	if time.Since(node.LastHB) > 15*time.Second {
		return false
	}
	if node.BWLimit > 0 && float64(node.BWUsed)/float64(node.BWLimit) > 0.7 {
		return false
	}
	if node.LossRate > 0.05 {
		return false
	}
	return true
}

// NodeHealthScore 综合健康度评分 (0-100)
// 参考 Envoy / Nginx Plus 的做法，比单纯阈值过滤更平滑
//
//	score = 0.4 × bw_score + 0.3 × rtt_score + 0.2 × quality_score + 0.1 × stability
//
// 其中 stability 需要外部传入（最近 1 小时的无故障时长比例）
func NodeHealthScore(node *NodeInfo, stability float64) float64 {
	// 带宽评分 (0-1): 带宽越空闲越好
	bwScore := 1.0
	if node.BWLimit > 0 {
		bwScore = 1.0 - float64(node.BWUsed)/float64(node.BWLimit)
		if bwScore < 0 {
			bwScore = 0
		}
	}

	// RTT 评分 (0-1)
	rttScore := calcLatencyScore(node.RTT)

	// 质量评分 (0-1)
	qualityScore := calcQualityScore(node.LossRate)

	// stability 限制在 0-1
	if stability < 0 {
		stability = 0
	}
	if stability > 1 {
		stability = 1
	}

	score := 0.4*bwScore + 0.3*rttScore + 0.2*qualityScore + 0.1*stability
	return score * 100 // 转为 0-100
}

// NodeScore 计算节点评分 —— 以实测指标为主
//
// 评分公式 (满分 1.0):
//
//	score = latency_score * load_score * quality_score
//
// 其中:
//   - latency_score: 延迟越低越好，RTT<50ms 满分，>500ms 趋近0
//   - load_score:    负载越低越好，0用户满分，接近上限趋近0
//   - quality_score: 丢包率越低越好，0%满分，>3%趋近0
//
// 地域匹配不参与评分，由调度器在同分段内做优先排序
func NodeScore(node *NodeInfo) float64 {
	latencyScore := calcLatencyScore(node.RTT)
	loadScore := calcLoadScore(node.OnlineUsers, node.BWUsed, node.BWLimit)
	qualityScore := calcQualityScore(node.LossRate)

	return latencyScore * loadScore * qualityScore
}

// NodeScoreWithRegionBonus 地域匹配加分版
// 同区同运营商 +20%，同区 +10%，不同区不加分
// 加分是在基础分上乘一个系数，不会让烂节点因为地域匹配变好
func NodeScoreWithRegionBonus(node *NodeInfo, clientRegion, clientISP string) float64 {
	base := NodeScore(node)
	if base <= 0 {
		return 0
	}

	bonus := 1.0
	if node.Region == clientRegion {
		if node.ISP == clientISP {
			bonus = 1.20 // 同区同运营商 +20%
		} else {
			bonus = 1.10 // 同区不同运营商 +10%
		}
	}

	return base * bonus
}

// --- 评分子函数 ---

// calcLatencyScore RTT 评分 (0~1)
//
//	<20ms   → 1.0
//	20-50ms → 0.9-1.0
//	50-100ms → 0.7-0.9
//	100-200ms → 0.4-0.7
//	200-500ms → 0.1-0.4
//	>500ms  → 趋近0
func calcLatencyScore(rttMs int) float64 {
	if rttMs <= 0 {
		return 1.0 // 未测量，给满分（不惩罚）
	}
	if rttMs <= 20 {
		return 1.0
	}
	if rttMs <= 50 {
		return 1.0 - float64(rttMs-20)/100.0
	}
	if rttMs <= 100 {
		return 0.9 - float64(rttMs-50)/166.0
	}
	if rttMs <= 200 {
		return 0.7 - float64(rttMs-100)/333.0
	}
	if rttMs <= 500 {
		return 0.4 - float64(rttMs-200)/1000.0
	}
	return 0.05 // >500ms 还活着就给个最低分
}

// calcLoadScore 负载评分 (0~1)
//
//	0用户 + 带宽空闲 → 1.0
//	带宽用到50%      → ~0.5
//	带宽用到70%      → ~0.3 (接近硬阈值)
func calcLoadScore(users int, bwUsed, bwLimit int64) float64 {
	score := 1.0

	// 用户数惩罚 (每个用户扣一点，但不是线性)
	if users > 0 {
		userPenalty := float64(users) / 500.0 // 500人时扣到0
		score -= userPenalty * 0.3 // 最多扣0.3
	}

	// 带宽占用惩罚
	if bwLimit > 0 {
		bwRatio := float64(bwUsed) / float64(bwLimit)
		// 指数惩罚: 占用越多扣得越狠
		score -= bwRatio * 0.7 // 最多扣0.7
	}

	if score < 0.05 {
		return 0.05
	}
	return score
}

// calcQualityScore 质量评分 (0~1)
//
//	0% 丢包 → 1.0
//	1% 丢包 → 0.85
//	3% 丢包 → 0.5
//	5% 丢包 → 0 (被硬阈值剔除)
func calcQualityScore(lossRate float64) float64 {
	if lossRate <= 0 {
		return 1.0
	}
	if lossRate >= 0.05 {
		return 0 // 应该被硬阈值剔除
	}
	// 指数衰减
	return (0.05 - lossRate) / 0.05 // 线性: 0%→1.0, 5%→0
}
