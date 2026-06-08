package controller

import (
	"fmt"
	"sync"
	"time"

	"github.com/user/live-cdn/internal/common"
)

// MemoryStore is an in-memory store for nodes, streams, sessions
// For production, replace with Redis
type MemoryStore struct {
	mu         sync.RWMutex
	nodes      map[string]*common.NodeInfo    // node_id -> NodeInfo
	streams    map[string]*common.StreamInfo  // stream_key -> StreamInfo
	sessions   map[string]*common.SessionInfo // session_id -> SessionInfo
	tokens     map[string]string              // viewer_token -> stream_key
	complaints map[string]*complaintWindow    // node_id -> complaint stats
}

type complaintWindow struct {
	count    int
	firstAt  time.Time
	cooldown time.Time
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		nodes:      make(map[string]*common.NodeInfo),
		streams:    make(map[string]*common.StreamInfo),
		sessions:   make(map[string]*common.SessionInfo),
		tokens:     make(map[string]string),
		complaints: make(map[string]*complaintWindow),
	}
}

// --- Node operations ---

func (s *MemoryStore) RegisterNode(req *common.RegisterRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 检查是否已存在的节点（重连场景）
	if existing, ok := s.nodes[req.NodeID]; ok {
		// 已存在的在线节点重连: 更新信息但保持状态
		existing.PublicIP = req.PublicIP
		existing.PublicIPv6 = req.PublicIPv6
		existing.Port = req.Port
		existing.Region = req.Region
		existing.ISP = req.ISP
		existing.BWLimit = req.BWLimit
		existing.Domain = req.Domain
		existing.Protocol = req.Protocol
		existing.WsPath = req.WsPath
		existing.TLSEnabled = req.TLSEnabled
		existing.LastHB = time.Now()
		// 如果之前是离线状态，改回在线
		if existing.Status == common.NodeStatusOffline {
			existing.Status = common.NodeStatusOnline
		}
		return nil
	}

	// 新节点: 默认进入 pending 状态，需要管理员审批
	node := &common.NodeInfo{
		NodeID:          req.NodeID,
		PublicIP:        req.PublicIP,
		PublicIPv6:      req.PublicIPv6,
		Port:            req.Port,
		Region:          req.Region,
		ISP:             req.ISP,
		BWLimit:         req.BWLimit,
		Domain:          req.Domain,
		Protocol:        req.Protocol,
		WsPath:          req.WsPath,
		TLSEnabled:      req.TLSEnabled,
		Status:          common.NodeStatusPending, // 新节点待审批
		LastHB:          time.Now(),
		CascadeEnabled:  req.CascadeEnabled,
		CascadeUpstream: req.CascadeUpstream,
		CascadeTier:     0,
		CascadeDepth:    0,
		ChildrenCount:   0,
		CascadeEgressBW: 0,
		CurrentUpstream: "",
		// 动态权重 (参考 Nginx 三权分离): 新节点从低权重开始慢启动
		EffectiveWeight:  0.5, // 新节点慢启动, 从50%权重开始逐步恢复到1.0
		ConsecutiveFails: 0,
	}
	s.nodes[req.NodeID] = node
	return nil
}

func (s *MemoryStore) UpdateHeartbeat(req *common.HeartbeatRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	node, ok := s.nodes[req.NodeID]
	if !ok {
		return fmt.Errorf("node %s not registered", req.NodeID)
	}
	node.BWUsed = req.BWUsed
	node.BWLimit = req.BWLimit
	node.OnlineUsers = req.OnlineUsers
	node.RTT = req.RTT
	node.LossRate = req.LossRate
	node.LastHB = time.Now()
	node.Status = common.NodeStatusOnline
	// Mesh 级联统计
	node.CascadeDepth = req.CascadeDepth
	node.ChildrenCount = req.ChildrenCount
	node.CascadeEgressBW = req.CascadeEgressBW
	node.CurrentUpstream = req.CurrentUpstream
	return nil
}

func (s *MemoryStore) GetHealthyNodes() []*common.NodeInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*common.NodeInfo
	for _, node := range s.nodes {
		if common.IsNodeHealthy(node) && !s.isCooling(node.NodeID) {
			result = append(result, node)
		}
	}
	return result
}

func (s *MemoryStore) GetNodesByRegion(region, isp string) []*common.NodeInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*common.NodeInfo
	for _, node := range s.nodes {
		if !common.IsNodeHealthy(node) || s.isCooling(node.NodeID) {
			continue
		}
		if node.Region == region && node.ISP == isp {
			result = append(result, node)
		}
	}
	return result
}

func (s *MemoryStore) GetNode(id string) (*common.NodeInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[id]
	return n, ok
}

func (s *MemoryStore) RemoveStaleNodes(timeout time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for id, node := range s.nodes {
		if time.Since(node.LastHB) > timeout {
			delete(s.nodes, id)
			removed++
		}
	}
	return removed
}

// --- Stream operations ---

func (s *MemoryStore) CreateStream(info *common.StreamInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streams[info.StreamKey] = info
}

func (s *MemoryStore) GetStream(key string) (*common.StreamInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	si, ok := s.streams[key]
	return si, ok
}

func (s *MemoryStore) EndStream(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if si, ok := s.streams[key]; ok {
		si.IsLive = false
	}
}

func (s *MemoryStore) GetLiveStreams() []*common.StreamInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*common.StreamInfo
	for _, si := range s.streams {
		if si.IsLive {
			result = append(result, si)
		}
	}
	return result
}

// --- Token operations ---

func (s *MemoryStore) SetToken(token, streamKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = streamKey
}

func (s *MemoryStore) ValidateToken(token string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sk, ok := s.tokens[token]
	return sk, ok
}

// --- Session operations ---

func (s *MemoryStore) CreateSession(info *common.SessionInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[info.SessionID] = info
}

func (s *MemoryStore) GetSession(id string) (*common.SessionInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	si, ok := s.sessions[id]
	return si, ok
}

func (s *MemoryStore) RemoveSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

func (s *MemoryStore) TouchSession(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok {
		return false
	}
	session.LastActive = time.Now()
	return true
}

func (s *MemoryStore) RemoveStaleSessions(timeout time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for id, session := range s.sessions {
		if time.Since(session.LastActive) > timeout {
			delete(s.sessions, id)
			removed++
		}
	}
	return removed
}

// --- Complaint / cooling operations ---

func (s *MemoryStore) RecordComplaint(nodeID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cw, ok := s.complaints[nodeID]
	if !ok {
		cw = &complaintWindow{}
		s.complaints[nodeID] = cw
	}

	if time.Since(cw.firstAt) > 5*time.Minute {
		cw.count = 0
		cw.firstAt = time.Now()
	}

	cw.count++

	// 3 complaints in 5 min -> 10 min cooldown
	if cw.count >= 3 {
		cw.cooldown = time.Now().Add(10 * time.Minute)
		cw.count = 0
	}
}

func (s *MemoryStore) isCooling(nodeID string) bool {
	cw, ok := s.complaints[nodeID]
	if !ok {
		return false
	}
	return time.Now().Before(cw.cooldown)
}

// GetAllNodes returns all registered nodes (for status page)
func (s *MemoryStore) GetAllNodes() []*common.NodeInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*common.NodeInfo, 0, len(s.nodes))
	for _, n := range s.nodes {
		result = append(result, n)
	}
	return result
}

// GetAllSessions returns all active sessions
func (s *MemoryStore) GetAllSessions() []*common.SessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*common.SessionInfo, 0, len(s.sessions))
	for _, si := range s.sessions {
		result = append(result, si)
	}
	return result
}

// RemoveNode removes a node by ID
func (s *MemoryStore) RemoveNode(nodeID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.nodes[nodeID]
	if ok {
		delete(s.nodes, nodeID)
	}
	return ok
}

// SetNodeStatus manually sets a node's status
func (s *MemoryStore) SetNodeStatus(nodeID string, status common.NodeStatus) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	node, ok := s.nodes[nodeID]
	if ok {
		node.Status = status
	}
	return ok
}

// GetAllStreams returns all streams (including ended)
func (s *MemoryStore) GetAllStreams() []*common.StreamInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*common.StreamInfo, 0, len(s.streams))
	for _, si := range s.streams {
		result = append(result, si)
	}
	return result
}

// SessionCount returns the number of active sessions
func (s *MemoryStore) SessionCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}
