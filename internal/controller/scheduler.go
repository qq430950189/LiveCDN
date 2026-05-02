package controller

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/user/live-cdn/internal/common"
)

// Scheduler handles node selection based on the dispatch algorithm
//
// 核心设计: 实测指标驱动，地域标识为辅
//
// 节点的 region/isp 是注册时由运维指定的（人工标注这台机器走的什么线），
// 不做自动探测——因为 NAT 挂机宝的线路、机房是固定的，只有运维知道。
//
// 调度流程:
// 1. 硬阈值过滤 (心跳超时/带宽>70%/丢包>5%/冷却中)
// 2. 异常检测 (参考 Envoy Outlier Detection: 连续失败>5次 → 驱逐, 30s 后慢恢复)
// 3. 会话粘性检查 (30min 同节点, 到期强制轮换)
// 4. 按实测指标评分 (延迟×负载×质量), 地域匹配做同分加分
// 5. P2C 选择 (参考 Envoy Least Request: 随机选2个候选, 取负载更低的)
//
// 动态权重 (参考 Nginx 三权分离):
// - weight: 静态基础权重 (= NodeScore, 基于 RTT/负载/质量/地域)
// - effective_weight: 动态有效权重 (失败衰减, 成功慢恢复)
// - 调度时用 effective_weight 而非静态 score, 实现故障降权和慢启动
type Scheduler struct {
	store  *MemoryStore
	geo    *IPLocator  // 只用于客户端 IP 查询
	sticky *StickyStore

	// 异常检测状态 (参考 Envoy Outlier Detection)
	outlierMu       sync.RWMutex
	outlierMap      map[string]*outlierState // nodeID → 异常状态
}

// outlierState 节点异常检测状态 (参考 Envoy OutlierDetection)
type outlierState struct {
	consecutiveFails int       // 连续失败计数
	ejectedUntil    time.Time // 被驱逐到何时 (0=未被驱逐)
	ejectCount      int       // 历史驱逐次数 (影响恢复时间)
	lastEjectAt     time.Time // 最近一次驱逐时间
}

const (
	outlierConsecutiveFails = 5                          // 连续失败阈值 (参考 Envoy consecutive_5xx)
	outlierBaseEjectTime    = 30 * time.Second           // 基础驱逐时间 (参考 Envoy base_ejection_time)
	outlierMaxEjectTime     = 300 * time.Second          // 最大驱逐时间 (5分钟)
	outlierMaxEjectPercent  = 0.3                         // 最大驱逐比例 30% (参考 Envoy max_ejection_percent)

	// 动态权重参数 (参考 Nginx effective_weight)
	weightDecayOnFail   = 0.5   // 失败时权重衰减系数 (effective_weight *= decay)
	weightRecoverStep   = 0.1   // 成功时权重恢复步长 (effective_weight += step, 上限1.0)
)

func NewScheduler(store *MemoryStore) *Scheduler {
	return &Scheduler{
		store:      store,
		geo:        NewIPLocator(),
		sticky:     NewStickyStore(),
		outlierMap: make(map[string]*outlierState),
	}
}

// DispatchResult holds the result of a dispatch operation
type DispatchResult struct {
	Nodes []common.NodeEndpoint
}

// Dispatch selects the best nodes for a viewer
func (s *Scheduler) Dispatch(streamKey, clientIP, clientRegion, clientISP string) (*DispatchResult, error) {
	// Step 0: 用 IP 库识别客户端地区 (仅用于客户端)
	if clientRegion == "" || clientISP == "" {
		geoRegion, geoISP := s.geo.Lookup(clientIP)
		if clientRegion == "" {
			clientRegion = geoRegion
		}
		if clientISP == "" {
			clientISP = geoISP
		}
	}

	// Step 1: 硬阈值过滤
	healthyNodes := s.store.GetHealthyNodes()
	if len(healthyNodes) == 0 {
		return nil, ErrNoAvailableNodes
	}

	// Step 2: 异常检测过滤 (参考 Envoy Outlier Detection)
	// 驱逐的节点不参与调度, 除非驱逐时间已过
	availableNodes := s.filterOutliers(healthyNodes)
	if len(availableNodes) == 0 {
		// 所有节点被驱逐 → 降级使用所有健康节点 (防止雪崩)
		log.Warn().Msg("所有节点被异常检测驱逐, 降级使用全部健康节点")
		availableNodes = healthyNodes
	}

	// Step 3: 会话粘性
	clientID := clientIP
	if stickyNodeID, mustRotate := s.sticky.Get(clientID); stickyNodeID != "" {
		if !mustRotate {
			if node, ok := s.store.GetNode(stickyNodeID); ok && common.IsNodeHealthy(node) && !s.isEjected(stickyNodeID) {
				return s.dispatchWithSticky(node, clientRegion, clientISP)
			}
		}
		s.sticky.Rotate(clientID)
	}

	// Step 4: 评分排序 —— 实测指标为主，地域为辅, 动态权重修正
	scored := s.scoreNodes(availableNodes, clientRegion, clientISP)

	log.Debug().
		Str("client", fmt.Sprintf("%s/%s", clientRegion, clientISP)).
		Int("candidates", len(scored)).
		Msg("dispatch scoring")

	// Step 5: P2C 选择 (参考 Envoy Least Request P2C + Nginx Random Two)
	// 随机选2个候选, 取有效权重更高的 → 兼顾负载均衡和随机性, 避免热点
	selected := s.p2cSelect(scored, 3)

	if len(selected) == 0 {
		return nil, ErrNoAvailableNodes
	}

	// 记录粘性
	s.sticky.Set(clientID, selected[0].NodeID)

	// 构建 endpoint
	endpoints := make([]common.NodeEndpoint, 0, len(selected))
	for i, node := range selected {
		ep := common.NodeEndpoint{
			Domain:          node.Domain,
			Port:            node.Port,
			Protocol:        node.Protocol,
			WsPath:          node.WsPath,
			Region:          node.Region,
			ISP:             node.ISP,
			Priority:        i,
			CascadeRelayURL: buildCascadeRelayURL(node),
			IPv6FLVURL:      buildIPv6FLVURL(node),
		}
		ep.URL = buildNodeURL(node)
		ep.FLVURL = buildNodeFLVURL(node)
		ep.HLSURL = buildNodeHLSURL(node)
		endpoints = append(endpoints, ep)
	}

	return &DispatchResult{Nodes: endpoints}, nil
}

// dispatchWithSticky 返回粘性节点为主 + 其他节点为备
func (s *Scheduler) dispatchWithSticky(stickyNode *common.NodeInfo, clientRegion, clientISP string) (*DispatchResult, error) {
	healthyNodes := s.store.GetHealthyNodes()
	availableNodes := s.filterOutliers(healthyNodes)

	var backups []*common.NodeInfo
	for _, n := range availableNodes {
		if n.NodeID != stickyNode.NodeID {
			backups = append(backups, n)
		}
	}

	result := []*common.NodeInfo{stickyNode}
	backupScored := s.scoreNodes(backups, clientRegion, clientISP)
	backupSelected := s.p2cSelect(backupScored, 2)
	result = append(result, backupSelected...)

	endpoints := make([]common.NodeEndpoint, 0, len(result))
	for i, node := range result {
		ep := common.NodeEndpoint{
			Domain:          node.Domain,
			Port:            node.Port,
			Protocol:        node.Protocol,
			WsPath:          node.WsPath,
			Region:          node.Region,
			ISP:             node.ISP,
			Priority:        i,
			CascadeRelayURL: buildCascadeRelayURL(node),
			IPv6FLVURL:      buildIPv6FLVURL(node),
		}
		ep.URL = buildNodeURL(node)
		ep.FLVURL = buildNodeFLVURL(node)
		ep.HLSURL = buildNodeHLSURL(node)
		endpoints = append(endpoints, ep)
	}

	return &DispatchResult{Nodes: endpoints}, nil
}

// scoreNodes 对节点评分排序
// 基础分 = 延迟分 × 负载分 × 质量分 (0~1)
// 地域匹配在同基础分上做加分 (同区同运营商 +20%, 同区 +10%)
// 动态权重修正: final_score = base_score × effective_weight
// 这意味着：一个持续失败的节点会被降权, 恢复中的节点逐步回到正常
func (s *Scheduler) scoreNodes(nodes []*common.NodeInfo, clientRegion, clientISP string) []scoredNode {
	scored := make([]scoredNode, len(nodes))
	for i, n := range nodes {
		baseScore := common.NodeScoreWithRegionBonus(n, clientRegion, clientISP)
		// 动态权重修正 (参考 Nginx effective_weight)
		ew := n.EffectiveWeight
		if ew <= 0 {
			ew = 0.1 // 最低权重, 不完全归零
		}
		finalScore := baseScore * ew
		scored[i] = scoredNode{node: n, score: finalScore, effectiveWeight: ew}
	}

	// 按分数降序
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	return scored
}

type scoredNode struct {
	node            *common.NodeInfo
	score           float64
	effectiveWeight float64
}

// p2cSelect Power-of-Two-Choices 选择 (参考 Envoy Least Request + Nginx Random Two)
//
// 比 weightedSelect 更好的负载均衡:
// - weightedSelect: 纯加权随机, 所有观众可能涌向最高分节点
// - P2C: 随机选2个, 取分更高的 → 兼顾均衡性和质量, 避免热点
//
// 具体实现: 先按权重随机选2个候选 (加权随机初筛), 然后取 effective_weight 更高的
// 这样既考虑了权重分布, 又避免了所有请求集中到同一节点
func (s *Scheduler) p2cSelect(nodes []scoredNode, n int) []*common.NodeInfo {
	if len(nodes) <= n {
		result := make([]*common.NodeInfo, len(nodes))
		for i, sn := range nodes {
			result[i] = sn.node
		}
		return result
	}

	selected := make([]*common.NodeInfo, 0, n)
	used := make(map[int]bool)

	for len(selected) < n {
		// P2C: 随机选2个候选
		candidates := s.pickTwoWeighted(nodes, used)
		if len(candidates) < 1 {
			break
		}
		if len(candidates) == 1 {
			selected = append(selected, candidates[0].node)
			for i, sn := range nodes {
				if sn.node.NodeID == candidates[0].node.NodeID {
					used[i] = true
					break
				}
			}
			continue
		}

		// 取 effective_weight 更高的 (参考 Nginx "random two" least_conn)
		winner := candidates[0]
		if candidates[1].effectiveWeight > candidates[0].effectiveWeight {
			winner = candidates[1]
		} else if candidates[1].effectiveWeight == candidates[0].effectiveWeight {
			// 同权重时取分更高的
			if candidates[1].score > candidates[0].score {
				winner = candidates[1]
			}
		}

		selected = append(selected, winner.node)
		for i, sn := range nodes {
			if sn.node.NodeID == winner.node.NodeID {
				used[i] = true
				break
			}
		}
	}

	return selected
}

// pickTwoWeighted 加权随机选2个不同候选
func (s *Scheduler) pickTwoWeighted(nodes []scoredNode, used map[int]bool) []scoredNode {
	available := make([]scoredNode, 0, len(nodes))
	totalWeight := 0.0
	for i, sn := range nodes {
		if !used[i] && sn.score > 0 {
			available = append(available, sn)
			totalWeight += sn.score
		}
	}
	if len(available) == 0 || totalWeight <= 0 {
		return nil
	}

	pickOne := func() *scoredNode {
		r := rand.Float64() * totalWeight
		cumulative := 0.0
		for i, sn := range available {
			cumulative += sn.score
			if r <= cumulative {
				totalWeight -= sn.score
				result := available[i]
				// 移除已选
				available = append(available[:i], available[i+1:]...)
				return &result
			}
		}
		// 浮点精度兜底
		if len(available) > 0 {
			result := available[0]
			totalWeight -= result.score
			available = available[1:]
			return &result
		}
		return nil
	}

	var result []scoredNode
	if p := pickOne(); p != nil {
		result = append(result, *p)
	}
	if p := pickOne(); p != nil {
		result = append(result, *p)
	}
	return result
}

// --- 异常检测 (参考 Envoy Outlier Detection) ---

// filterOutliers 过滤被驱逐的节点
func (s *Scheduler) filterOutliers(nodes []*common.NodeInfo) []*common.NodeInfo {
	s.outlierMu.RLock()
	defer s.outlierMu.RUnlock()

	now := time.Now()

	// 计算驱逐比例上限
	maxEject := int(float64(len(nodes)) * outlierMaxEjectPercent)
	ejected := 0

	var available []*common.NodeInfo
	for _, n := range nodes {
		state, ok := s.outlierMap[n.NodeID]
		if !ok || state.ejectedUntil.IsZero() {
			available = append(available, n)
			continue
		}
		// 驱逐时间已过 → 恢复
		if now.After(state.ejectedUntil) {
			available = append(available, n)
			continue
		}
		// 仍在驱逐期
		if ejected < maxEject {
			ejected++
			continue // 被驱逐
		}
		// 超过最大驱逐比例 → 不驱逐, 保留
		available = append(available, n)
	}

	return available
}

// isEjected 检查节点是否被异常检测驱逐
func (s *Scheduler) isEjected(nodeID string) bool {
	s.outlierMu.RLock()
	defer s.outlierMu.RUnlock()

	state, ok := s.outlierMap[nodeID]
	if !ok {
		return false
	}
	if state.ejectedUntil.IsZero() {
		return false
	}
	return time.Now().Before(state.ejectedUntil)
}

// ReportNodeFail 报告节点调度失败 (由质量上报触发)
// 参考 Envoy Outlier Detection: consecutive_5xx 驱逐 + 指数增长恢复时间
func (s *Scheduler) ReportNodeFail(nodeID string) {
	s.outlierMu.Lock()
	defer s.outlierMu.Unlock()

	state, ok := s.outlierMap[nodeID]
	if !ok {
		state = &outlierState{}
		s.outlierMap[nodeID] = state
	}

	state.consecutiveFails++
	state.lastEjectAt = time.Now()

	// 更新节点的动态权重 (参考 Nginx effective_weight 衰减)
	node, ok := s.store.GetNode(nodeID)
	if ok {
		newEW := node.EffectiveWeight * weightDecayOnFail
		if newEW < 0.1 {
			newEW = 0.1
		}
		node.EffectiveWeight = newEW
		node.ConsecutiveFails = state.consecutiveFails
		node.LastFailAt = time.Now()
	}

	// 连续失败超过阈值 → 驱逐
	if state.consecutiveFails >= outlierConsecutiveFails {
		state.ejectCount++
		// 驱逐时间 = base × ejectCount, 上限 maxEjectTime (参考 Envoy)
		ejectDur := outlierBaseEjectTime * time.Duration(state.ejectCount)
		if ejectDur > outlierMaxEjectTime {
			ejectDur = outlierMaxEjectTime
		}
		state.ejectedUntil = time.Now().Add(ejectDur)

		log.Warn().
			Str("node_id", nodeID).
			Int("consecutive_fails", state.consecutiveFails).
			Int("eject_count", state.ejectCount).
			Dur("eject_duration", ejectDur).
			Msg("节点被异常检测驱逐")
	}
}

// ReportNodeSuccess 报告节点调度成功
// 参考 Nginx effective_weight 慢恢复: effective_weight += step
func (s *Scheduler) ReportNodeSuccess(nodeID string) {
	s.outlierMu.Lock()
	defer s.outlierMu.Unlock()

	state, ok := s.outlierMap[nodeID]
	if ok {
		state.consecutiveFails = 0
		// 如果在驱逐中且已到时间, 清除驱逐
		if !state.ejectedUntil.IsZero() && time.Now().After(state.ejectedUntil) {
			state.ejectedUntil = time.Time{}
			state.ejectCount = 0 // 恢复后重置驱逐计数
		}
	}

	// 慢恢复 effective_weight (参考 Nginx: effective_weight++ 直到恢复到 weight)
	node, ok := s.store.GetNode(nodeID)
	if ok && node.EffectiveWeight < 1.0 {
		node.EffectiveWeight += weightRecoverStep
		if node.EffectiveWeight > 1.0 {
			node.EffectiveWeight = 1.0
		}
		node.ConsecutiveFails = 0
	}
}

// RecoverOutliers 周期性恢复被驱逐的节点 (由 cleanupLoop 调用)
// 参考 Envoy: 驱逐时间到期后自动恢复, effective_weight 逐步恢复
func (s *Scheduler) RecoverOutliers() {
	s.outlierMu.Lock()
	defer s.outlierMu.Unlock()

	now := time.Now()
	for nodeID, state := range s.outlierMap {
		// 驱逐到期 → 清除驱逐状态
		if !state.ejectedUntil.IsZero() && now.After(state.ejectedUntil) {
			state.ejectedUntil = time.Time{}
			state.consecutiveFails = 0
			log.Info().Str("node_id", nodeID).Msg("异常检测驱逐到期, 节点恢复")
		}
	}
}

// weightedSelect 加权随机选取 (保留作为 fallback)
func (s *Scheduler) weightedSelect(nodes []scoredNode, n int) []*common.NodeInfo {
	if len(nodes) <= n {
		result := make([]*common.NodeInfo, len(nodes))
		for i, sn := range nodes {
			result[i] = sn.node
		}
		return result
	}

	totalWeight := 0.0
	for _, sn := range nodes {
		if sn.score > 0 {
			totalWeight += sn.score
		}
	}

	selected := make([]*common.NodeInfo, 0, n)
	used := make(map[int]bool)

	for len(selected) < n && totalWeight > 0 {
		r := rand.Float64() * totalWeight
		cumulative := 0.0
		for i, sn := range nodes {
			if used[i] || sn.score <= 0 {
				continue
			}
			cumulative += sn.score
			if r <= cumulative {
				selected = append(selected, sn.node)
				totalWeight -= sn.score
				used[i] = true
				break
			}
		}
		// Float precision fallback
		if len(selected) < len(used)+1 {
			for i, sn := range nodes {
				if !used[i] && sn.score > 0 {
					selected = append(selected, sn.node)
					totalWeight -= sn.score
					used[i] = true
					break
				}
			}
		}
	}

	return selected
}

func buildNodeURL(node *common.NodeInfo) string {
	scheme := "https"
	if !node.TLSEnabled {
		scheme = "http"
	}
	switch node.Protocol {
	case "ws", "wss":
		wsScheme := "wss"
		if !node.TLSEnabled {
			wsScheme = "ws"
		}
		path := node.WsPath
		if path == "" {
			path = "/ws/live"
		}
		return wsScheme + "://" + node.Domain + path
	case "grpc":
		return scheme + "://" + node.Domain + "/grpc/live"
	default:
		return scheme + "://" + node.Domain + "/live/stream.m3u8"
	}
}

// buildNodeFLVURL 构建 HTTP-FLV 播放地址
func buildNodeFLVURL(node *common.NodeInfo) string {
	scheme := "https"
	if !node.TLSEnabled {
		scheme = "http"
	}
	host := node.Domain
	if node.Port != 0 && node.Port != 80 && node.Port != 443 {
		host = fmt.Sprintf("%s:%d", node.Domain, node.Port)
	}
	return scheme + "://" + host + "/live/{stream_key}.flv"
}

// buildCascadeRelayURL 构建级联拉流地址
// 级联下游 Agent 从上游 Agent 拉流: http://上游Agent:9090/live/{stream_key}.flv
// 核心<->Agent 走 IPv6 时使用 IPv6 地址
func buildCascadeRelayURL(node *common.NodeInfo) string {
	scheme := "http"
	if node.TLSEnabled {
		scheme = "https"
	}
	// 优先用 IPv6 (Agent 间通信走 IPv6 更便宜)
	host := node.PublicIP
	if node.PublicIPv6 != "" {
		host = "[" + node.PublicIPv6 + "]"
	}
	if node.Port != 0 && node.Port != 80 && node.Port != 443 {
		host = fmt.Sprintf("%s:%d", host, node.Port)
	} else if node.PublicIPv6 != "" {
		host = "[" + node.PublicIPv6 + "]"
	}
	return scheme + "://" + host + "/live/{stream_key}.flv"
}

// buildIPv6FLVURL 构建纯 IPv6 FLV 地址 (Agent 间级联用)
func buildIPv6FLVURL(node *common.NodeInfo) string {
	if node.PublicIPv6 == "" {
		return ""
	}
	scheme := "http"
	if node.TLSEnabled {
		scheme = "https"
	}
	host := "[" + node.PublicIPv6 + "]"
	if node.Port != 0 && node.Port != 80 {
		host = fmt.Sprintf("[%s]:%d", node.PublicIPv6, node.Port)
	}
	return scheme + "://" + host + "/live/{stream_key}.flv"
}

// buildNodeHLSURL 构建 LL-HLS 播放地址
func buildNodeHLSURL(node *common.NodeInfo) string {
	scheme := "https"
	if !node.TLSEnabled {
		scheme = "http"
	}
	host := node.Domain
	if node.Port != 0 && node.Port != 80 && node.Port != 443 {
		host = fmt.Sprintf("%s:%d", node.Domain, node.Port)
	}
	return scheme + "://" + host + "/live/{stream_key}/stream.m3u8"
}

var ErrNoAvailableNodes = &DispatchError{Msg: "no available nodes"}

type DispatchError struct {
	Msg string
}

func (e *DispatchError) Error() string {
	return e.Msg
}
