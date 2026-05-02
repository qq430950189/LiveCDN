package controller

// Agent Mesh 组网 — Controller 控制拓扑 + Agent 本地择优
//
// 核心架构 (参考 Tailscale 但针对直播CDN优化):
//   Controller: 生成全局拓扑约束、候选上游列表、级联层级、负载预算
//   Agent: 只在 Controller 允许的候选上游中做本地测速和切换
//
// 关键改进 (相比全量自治):
//   1. 不全量下发 mesh_map → 改为 candidate_upstreams (Agent只需知道自己的候选)
//   2. Tier 分层 (三层模型) → 禁止同层或反向拉流, 天然防环路
//   3. mesh_epoch → 增量版本号, 后续支持 delta map (参考 Envoy Delta xDS)
//   4. MeshPolicy → 切换策略由 Controller 下发, Agent 执行 (参考 Envoy xDS)
//   5. 容量预算 → cascade_egress_bw 与 viewer_bw 分开统计
//   6. 6维评分 → 含 topology 维度 (参考 Tailscale betterAddr 网络加分)
//   7. 迟滞控制 → 参考 Tailscale hysteresis 的综合评分+1%门限
//
// 级联层级 (三层模型):
//   Tier0: Origin (核心主机)
//   Tier1: 优选CDN — 直连 Origin, 同时服务用户和级联给其他 Agent
//   Tier2: 边缘CDN — 从 Tier1 拉流, 服务用户

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/user/live-cdn/internal/common"
)

// --- Mesh 数据结构 ---

// CandidateUpstream Controller 为 Agent 推荐的候选上游
type CandidateUpstream struct {
	NodeID         string  `json:"node_id"`          // 上游Agent ID
	URL            string  `json:"url"`              // 级联拉流地址 (含/cascade/路径)
	Region         string  `json:"region"`           // 地域
	ISP            string  `json:"isp"`              // 运营商
	Tier           int     `json:"tier"`             // 上游层级 (必须 < 自己的层级)
	ScoreHint      float64 `json:"score_hint"`       // Controller 评分提示 (0~100)
	MaxChildren    int     `json:"max_children"`     // 最大下游数
	CurrentChildren int   `json:"current_children"` // 当前下游数
	BWRemainingMbps float64 `json:"bw_remaining_mbps"` // 剩余级联带宽 (Mbps)
	AllowCascade   bool    `json:"allow_cascade"`    // 仍允许级联
	HasIPv6        bool    `json:"has_ipv6"`         // 是否有IPv6
	IPv6URL        string  `json:"ipv6_url,omitempty"` // IPv6级联地址
}

// MeshPolicy Controller 下发的切换策略
type MeshPolicy struct {
	MaxDepth              int `json:"max_depth"`                // 最大级联深度, 默认2
	SwitchCooldownSec     int `json:"switch_cooldown_sec"`      // 切换冷却时间(秒), 默认30
	MinScoreImprovement   int `json:"min_score_improvement"`    // 最低改善分数才切换, 默认15
	MeshPingIntervalSec   int `json:"mesh_ping_interval_sec"`   // Mesh Ping 间隔(秒), 默认10
	CandidateTTLSec       int `json:"candidate_ttl_sec"`        // 候选列表TTL(秒), 默认60
	MaxSwitchPer5Min      int `json:"max_switch_per_5min"`      // 5分钟内最大切换次数, 默认3
	HardFailTimeoutMs     int `json:"hard_fail_timeout_ms"`     // 上游硬故障超时(ms), 默认3000
}

// OriginFallback Origin 回退配置
type OriginFallback struct {
	URL      string `json:"url"`      // Origin FLV 地址
	Priority int    `json:"priority"` // 优先级 (999=最低)
}

// MeshMapResponse 心跳响应中的 Mesh 拓扑信息
// 类似 Tailscale MapResponse, 但只包含当前 Agent 的候选上游
type MeshMapResponse struct {
	MeshEpoch          uint64              `json:"mesh_epoch"`           // 拓扑版本号
	SelfNodeID         string              `json:"self_node_id"`         // 本节点ID
	SelfTier           int                 `json:"self_tier"`            // 本节点层级
	CandidateUpstreams []CandidateUpstream `json:"candidate_upstreams"`  // 候选上游列表
	OriginFallback     OriginFallback      `json:"origin_fallback"`      // Origin回退
	Policy             MeshPolicy          `json:"policy"`               // 切换策略
	MigrationAdvice    *MigrationAdvice    `json:"migration_advice,omitempty"` // 迁移建议
}

// MigrationAdvice Controller 下发的迁移建议
type MigrationAdvice struct {
	ShouldSwitch  bool   `json:"should_switch"`   // 是否应该切换
	PreferredNode string `json:"preferred_node"`  // 建议切换到的节点ID
	Reason        string `json:"reason"`          // 原因: parent_overloaded / topology_rebalance / ...
}

// DefaultMeshPolicy 默认切换策略
func DefaultMeshPolicy() MeshPolicy {
	return MeshPolicy{
		MaxDepth:              2,
		SwitchCooldownSec:     30,
		MinScoreImprovement:   15,
		MeshPingIntervalSec:   10,
		CandidateTTLSec:       60,
		MaxSwitchPer5Min:      3,
		HardFailTimeoutMs:     3000,
	}
}

// --- Mesh 构建 ---

// meshEpoch 全局拓扑版本号 (每次节点变化递增)
var meshEpoch uint64

func nextMeshEpoch() uint64 {
	meshEpoch++
	return meshEpoch
}

// buildMeshMapResponse 为指定 Agent 构建 Mesh 拓扑响应
// 只包含该 Agent 的候选上游, 不下发全网信息
func (s *Server) buildMeshMapResponse(requesterID string) *MeshMapResponse {
	requester, ok := s.store.GetNode(requesterID)
	if !ok {
		return nil
	}

	// 确定本节点层级
	selfTier := computeTier(requester)

	// 获取所有健康节点 (不含自己)
	healthyNodes := s.store.GetHealthyNodes()

	// 构建候选上游: 只选 tier < selfTier 的 cascade_enabled 节点
	candidates := make([]CandidateUpstream, 0)
	for _, n := range healthyNodes {
		if n.NodeID == requesterID {
			continue
		}
		if !n.CascadeEnabled {
			continue
		}
		upstreamTier := computeTier(n)
		// 关键规则: 只能从更低层级的节点拉流, 禁止同层或反向
		if upstreamTier >= selfTier {
			continue
		}

		candidate := buildCandidate(n, upstreamTier, requester)
		if candidate != nil {
			candidates = append(candidates, *candidate)
		}
	}

	// 按评分降序排序
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ScoreHint > candidates[j].ScoreHint
	})

	// 最多保留 3 个候选 (1 同区同ISP + 1 同区 + 1 兜底)
	if len(candidates) > 3 {
		candidates = candidates[:3]
	}

	// Origin 回退
	originURL := fmt.Sprintf("%s/live/{stream_key}.flv", s.cfg.OriginAddr)

	// 检查是否需要迁移建议
	migration := s.checkMigrationAdvice(requester, candidates)

	return &MeshMapResponse{
		MeshEpoch:          nextMeshEpoch(),
		SelfNodeID:         requesterID,
		SelfTier:           selfTier,
		CandidateUpstreams: candidates,
		OriginFallback: OriginFallback{
			URL:      originURL,
			Priority: 999,
		},
		Policy:          DefaultMeshPolicy(),
		MigrationAdvice:  migration,
	}
}

// computeTier 计算节点的 Tier 层级 (三层模型)
// Tier0: Origin (不可变)
// Tier1: 优选CDN — cascade_enabled + 直连Origin (cascade_upstream=false)
// Tier2: 边缘CDN — 从Tier1拉流或普通边缘节点
//
// 关键规则: 只能从更低 tier 的节点拉流
// Tier2 可以从 Tier1 拉 ✓
// Tier1 不能从 Tier1 拉 ✗ (同层互拉会形成环路)
func computeTier(node *common.NodeInfo) int {
	if node.CascadeEnabled && !node.CascadeUpstream {
		// Tier1: 优选CDN, 允许别人从自己拉, 自己直连Origin
		return 1
	}
	// Tier2: 边缘CDN, 从Tier1拉流或普通边缘节点
	return 2
}

// buildCandidate 从 NodeInfo 构建 CandidateUpstream
func buildCandidate(node *common.NodeInfo, tier int, requester *common.NodeInfo) *CandidateUpstream {
	loadScore := 0.0
	if node.BWLimit > 0 {
		loadScore = float64(node.BWUsed) / float64(node.BWLimit)
	}

	// 负载 > 70% 不推荐
	if loadScore > 0.70 {
		return nil
	}

	// 级联带宽预算
	var maxCascadeBW, usedCascadeBW, bwRemaining float64
	if node.BWLimit > 0 {
		maxCascadeBW = float64(node.BWLimit) * 0.30 / 125000.0 // 30% for cascade, bytes→Mbps
		usedCascadeBW = float64(node.CascadeEgressBW) / 125000.0
		bwRemaining = maxCascadeBW - usedCascadeBW
	} else {
		// 新节点未上报带宽, 假设 30Mbps 总量, 30% 给级联
		bwRemaining = 9.0
	}
	if bwRemaining < 2.0 { // 至少留 2Mbps
		return nil
	}

	// 最大下游数 (基于带宽: 每个下游 ~4Mbps)
	maxChildren := int(bwRemaining / 4.0)
	if maxChildren < 1 {
		return nil
	}

	// Controller 评分提示 (0~100)
	scoreHint := meshNodeScoreV2(node, requester.Region, requester.ISP)

	// 级联 URL (使用 /cascade/ 端点)
	cascadeURL := buildCascadeEndpointURL(node)

	return &CandidateUpstream{
		NodeID:          node.NodeID,
		URL:             cascadeURL,
		Region:          node.Region,
		ISP:             node.ISP,
		Tier:            tier,
		ScoreHint:       scoreHint * 100,
		MaxChildren:     maxChildren,
		CurrentChildren: node.ChildrenCount,
		BWRemainingMbps: math.Round(bwRemaining*100) / 100,
		AllowCascade:    true,
		HasIPv6:         node.PublicIPv6 != "",
		IPv6URL:         buildIPv6CascadeURL(node),
	}
}

// buildCascadeEndpointURL 构建级联专用端点 URL
// Agent 从 Agent 拉流使用 /cascade/ 而非 /live/
func buildCascadeEndpointURL(node *common.NodeInfo) string {
	scheme := "http"
	if node.TLSEnabled {
		scheme = "https"
	}
	host := node.PublicIP
	if node.PublicIPv6 != "" {
		host = "[" + node.PublicIPv6 + "]"
	}
	if node.Port != 0 && node.Port != 80 && node.Port != 443 {
		host = fmt.Sprintf("%s:%d", host, node.Port)
	}
	return scheme + "://" + host + "/cascade/{stream_key}.flv"
}

// buildIPv6CascadeURL 构建纯 IPv6 级联 URL
func buildIPv6CascadeURL(node *common.NodeInfo) string {
	if node.PublicIPv6 == "" {
		return ""
	}
	scheme := "http"
	if node.TLSEnabled {
		scheme = "https"
	}
	host := fmt.Sprintf("[%s]:%d", node.PublicIPv6, node.Port)
	return scheme + "://" + host + "/cascade/{stream_key}.flv"
}

// meshNodeScoreV2 综合评分 (0~1)
// 改进版 (参考 Tailscale betterAddr + Envoy ORCA):
//   6 维评分, Controller 给 hint, Agent 本地测速后加 latency 维度
//
//   score = 0.25*locality + 0.20*isp + 0.15*capacity + 0.15*stability + 0.10*ipv6 + 0.15*topology
//
// 新增 topology 维度 (参考 Tailscale betterAddr):
//   - 同机房 +30% (PrivateIP, 拓扑最近)
//   - 同区 +15% (同城部署)
//   - IPv6 +10% (免费内网流量)
//   - Tier1 优先 +5% (直连 Origin 的节点更稳定)
//
// latency 由 Agent 本地测速填入, Controller 只给其他 5 维的 hint
func meshNodeScoreV2(node *common.NodeInfo, requesterRegion, requesterISP string) float64 {
	locality := 0.0
	if node.Region == requesterRegion && node.ISP == requesterISP {
		locality = 1.0 // 同区同ISP
	} else if node.Region == requesterRegion {
		locality = 0.6 // 同区不同ISP
	} else {
		locality = 0.2 // 跨区
	}

	isp := 0.0
	if node.ISP == requesterISP {
		isp = 1.0
	} else {
		isp = 0.4 // 跨ISP
	}

	capacity := 0.5 // 默认中等容量
	if node.BWLimit > 0 {
		avail := 1.0 - float64(node.BWUsed)/float64(node.BWLimit)
		childrenAvail := 1.0
		if node.ChildrenCount > 0 {
			childrenAvail = 0.5
		}
		capacity = avail*0.6 + childrenAvail*0.4
	}

	stability := 0.8 // 默认稳定
	if node.LossRate > 0.05 {
		stability = 0.3
	} else if node.LossRate > 0.01 {
		stability = 0.6
	}

	ipv6 := 0.0
	if node.PublicIPv6 != "" {
		ipv6 = 1.0
	}

	// 拓扑偏好 (参考 Tailscale betterAddr 的网络加分体系)
	topology := 0.0
	if node.CascadeEnabled && !node.CascadeUpstream {
		topology += 0.3 // Tier1 直连 Origin, 更稳定 (类似 Tailscale Loopback +50)
	}
	if node.Region == requesterRegion && node.ISP == requesterISP {
		topology += 0.5 // 同机房/同区同ISP (类似 Tailscale Private IP +20)
	} else if node.Region == requesterRegion {
		topology += 0.2 // 同区 (类似 Tailscale LinkLocal +30)
	}
	if node.PublicIPv6 != "" {
		topology += 0.1 // IPv6 加分 (类似 Tailscale IPv6 +10)
	}
	if topology > 1.0 {
		topology = 1.0
	}

	score := 0.25*locality + 0.20*isp + 0.15*capacity + 0.15*stability + 0.10*ipv6 + 0.15*topology

	if score > 1.0 {
		score = 1.0
	}
	return score
}

// checkMigrationAdvice 检查是否需要建议 Agent 迁移
func (s *Server) checkMigrationAdvice(requester *common.NodeInfo, candidates []CandidateUpstream) *MigrationAdvice {
	// 如果当前上游仍然可用, 不建议迁移
	if requester.CurrentUpstream == "" {
		return nil
	}

	// 检查当前上游是否过载
	upstream, ok := s.store.GetNode(requester.CurrentUpstream)
	if !ok {
		return &MigrationAdvice{
			ShouldSwitch:  true,
			PreferredNode: "",
			Reason:        "upstream_offline",
		}
	}

	// 上游过载: 建议 migrate
	if upstream.BWLimit > 0 && float64(upstream.BWUsed)/float64(upstream.BWLimit) > 0.8 {
		if len(candidates) > 0 {
			return &MigrationAdvice{
				ShouldSwitch:  true,
				PreferredNode: candidates[0].NodeID,
				Reason:        "parent_overloaded",
			}
		}
	}

	return nil
}

// --- Store 扩展 ---

// UpdateCascadeStats 更新心跳中的级联统计
func (s *MemoryStore) UpdateCascadeStats(nodeID string, depth, childrenCount int, cascadeEgressBW int64, currentUpstream string, streamLagMs int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	node, ok := s.nodes[nodeID]
	if !ok {
		return
	}
	node.CascadeDepth = depth
	node.ChildrenCount = childrenCount
	node.CascadeEgressBW = cascadeEgressBW
	node.CurrentUpstream = currentUpstream
}

func unixMillis() int64 {
	return time.Now().UnixMilli()
}
