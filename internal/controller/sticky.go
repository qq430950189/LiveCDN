package controller

import (
	"sync"
	"time"
)

const (
	stickyTTL      = 30 * time.Minute  // 粘性有效期
	rotateInterval = 30 * time.Minute  // 强制轮换间隔
)

// StickyStore 管理客户端→节点的粘性映射
// 同一 uid 在 30 分钟内保持同一节点，到期后强制重新调度
type StickyStore struct {
	mu     sync.RWMutex
	entries map[string]*stickyEntry
}

type stickyEntry struct {
	nodeID    string
	bindAt    time.Time  // 绑定时间
	lastSeen  time.Time  // 最后调度时间
}

func NewStickyStore() *StickyStore {
	return &StickyStore{
		entries: make(map[string]*stickyEntry),
	}
}

// Get 获取客户端的粘性节点
// 返回 (nodeID, shouldForceRotate)
func (s *StickyStore) Get(clientID string) (nodeID string, mustRotate bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	e, ok := s.entries[clientID]
	if !ok {
		return "", false
	}

	// 检查是否需要强制轮换
	if time.Since(e.bindAt) > rotateInterval {
		return e.nodeID, true
	}

	return e.nodeID, false
}

// Set 设置客户端的粘性节点
func (s *StickyStore) Set(clientID, nodeID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 如果已有绑定且未过期，保持原绑定
	if e, ok := s.entries[clientID]; ok {
		if time.Since(e.bindAt) < rotateInterval && e.nodeID == nodeID {
			e.lastSeen = time.Now()
			return
		}
	}

	// 新绑定或轮换后重新绑定
	s.entries[clientID] = &stickyEntry{
		nodeID:   nodeID,
		bindAt:   time.Now(),
		lastSeen: time.Now(),
	}
}

// Rotate 强制轮换客户端的粘性 (删除旧绑定)
func (s *StickyStore) Rotate(clientID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, clientID)
}

// Cleanup 清理过期条目
func (s *StickyStore) Cleanup() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for id, e := range s.entries {
		if time.Since(e.lastSeen) > stickyTTL {
			delete(s.entries, id)
			removed++
		}
	}
	return removed
}
