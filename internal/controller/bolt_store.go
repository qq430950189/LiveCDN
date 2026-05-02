package controller

import (
	"encoding/json"
	"time"

	"github.com/rs/zerolog/log"
	bolt "go.etcd.io/bbolt"
	"github.com/user/live-cdn/internal/common"
)

// Store 接口 — MemoryStore 和 BoltStore 实现统一接口
// 使 Controller 重启后可从 bbolt 恢复状态

var (
	bucketNodes    = []byte("nodes")
	bucketStreams  = []byte("streams")
	bucketTokens   = []byte("tokens")
	bucketSessions = []byte("sessions")
)

// BoltStore 基于 bbolt 的持久化存储
type BoltStore struct {
	db    *bolt.DB
	mem   *MemoryStore // 内存缓存, 所有读操作走内存
	dbPath string
}

func NewBoltStore(dbPath string) (*BoltStore, error) {
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return nil, err
	}

	// 创建 buckets
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketNodes, bucketStreams, bucketTokens, bucketSessions} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}

	bs := &BoltStore{
		db:     db,
		mem:    NewMemoryStore(),
		dbPath: dbPath,
	}

	// 从 bbolt 恢复到内存
	if err := bs.restore(); err != nil {
		log.Warn().Err(err).Msg("failed to restore from bbolt, starting fresh")
	}

	return bs, nil
}

// Close 关闭数据库
func (bs *BoltStore) Close() error {
	return bs.db.Close()
}

// SaveSnapshot 将内存状态持久化到 bbolt
func (bs *BoltStore) SaveSnapshot() error {
	return bs.db.Update(func(tx *bolt.Tx) error {
		// 保存节点
		nodes := bs.mem.GetAllNodes()
		if b := tx.Bucket(bucketNodes); b != nil {
			// 先清空
			_ = b.ForEach(func(k, _ []byte) error {
				return b.Delete(k)
			})
			for _, n := range nodes {
				data, err := json.Marshal(n)
				if err != nil {
					continue
				}
				_ = b.Put([]byte(n.NodeID), data)
			}
		}

		// 保存流
		streams := bs.mem.GetAllStreams()
		if b := tx.Bucket(bucketStreams); b != nil {
			_ = b.ForEach(func(k, _ []byte) error {
				return b.Delete(k)
			})
			for _, s := range streams {
				data, err := json.Marshal(s)
				if err != nil {
					continue
				}
				_ = b.Put([]byte(s.StreamKey), data)
			}
		}

		return nil
	})
}

// restore 从 bbolt 恢复到内存
func (bs *BoltStore) restore() error {
	return bs.db.View(func(tx *bolt.Tx) error {
		// 恢复节点
		if b := tx.Bucket(bucketNodes); b != nil {
			count := 0
			_ = b.ForEach(func(k, v []byte) error {
				var node common.NodeInfo
				if err := json.Unmarshal(v, &node); err != nil {
					return nil
				}
				// 重启后心跳肯定超时, 标记为 offline
				if time.Since(node.LastHB) > 15*time.Second {
					node.Status = common.NodeStatusOffline
				}
				bs.mem.mu.Lock()
				bs.mem.nodes[node.NodeID] = &node
				bs.mem.mu.Unlock()
				count++
				return nil
			})
			if count > 0 {
				log.Info().Int("count", count).Msg("restored nodes from bbolt")
			}
		}

		// 恢复流
		if b := tx.Bucket(bucketStreams); b != nil {
			count := 0
			_ = b.ForEach(func(k, v []byte) error {
				var stream common.StreamInfo
				if err := json.Unmarshal(v, &stream); err != nil {
					return nil
				}
				bs.mem.mu.Lock()
				bs.mem.streams[stream.StreamKey] = &stream
				bs.mem.mu.Unlock()
				count++
				return nil
			})
			if count > 0 {
				log.Info().Int("count", count).Msg("restored streams from bbolt")
			}
		}

		return nil
	})
}

// StartSaveLoop 启动定期持久化 (每 30 秒)
func (bs *BoltStore) StartSaveLoop() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if err := bs.SaveSnapshot(); err != nil {
				log.Error().Err(err).Msg("failed to save snapshot to bbolt")
			}
		}
	}()
}

// --- 代理到 MemoryStore 的所有方法 ---

func (bs *BoltStore) RegisterNode(req *common.RegisterRequest) error {
	return bs.mem.RegisterNode(req)
}

func (bs *BoltStore) UpdateHeartbeat(req *common.HeartbeatRequest) error {
	return bs.mem.UpdateHeartbeat(req)
}

func (bs *BoltStore) GetHealthyNodes() []*common.NodeInfo {
	return bs.mem.GetHealthyNodes()
}

func (bs *BoltStore) GetNodesByRegion(region, isp string) []*common.NodeInfo {
	return bs.mem.GetNodesByRegion(region, isp)
}

func (bs *BoltStore) GetNode(id string) (*common.NodeInfo, bool) {
	return bs.mem.GetNode(id)
}

func (bs *BoltStore) RemoveStaleNodes(timeout time.Duration) int {
	return bs.mem.RemoveStaleNodes(timeout)
}

func (bs *BoltStore) RemoveNode(nodeID string) bool {
	return bs.mem.RemoveNode(nodeID)
}

func (bs *BoltStore) SetNodeStatus(nodeID string, status common.NodeStatus) bool {
	return bs.mem.SetNodeStatus(nodeID, status)
}

func (bs *BoltStore) GetAllNodes() []*common.NodeInfo {
	return bs.mem.GetAllNodes()
}

func (bs *BoltStore) CreateStream(info *common.StreamInfo) {
	bs.mem.CreateStream(info)
}

func (bs *BoltStore) GetStream(key string) (*common.StreamInfo, bool) {
	return bs.mem.GetStream(key)
}

func (bs *BoltStore) EndStream(key string) {
	bs.mem.EndStream(key)
}

func (bs *BoltStore) GetLiveStreams() []*common.StreamInfo {
	return bs.mem.GetLiveStreams()
}

func (bs *BoltStore) GetAllStreams() []*common.StreamInfo {
	return bs.mem.GetAllStreams()
}

func (bs *BoltStore) SetToken(token, streamKey string) {
	bs.mem.SetToken(token, streamKey)
}

func (bs *BoltStore) ValidateToken(token string) (string, bool) {
	return bs.mem.ValidateToken(token)
}

func (bs *BoltStore) CreateSession(info *common.SessionInfo) {
	bs.mem.CreateSession(info)
}

func (bs *BoltStore) GetSession(id string) (*common.SessionInfo, bool) {
	return bs.mem.GetSession(id)
}

func (bs *BoltStore) RemoveSession(id string) {
	bs.mem.RemoveSession(id)
}

func (bs *BoltStore) GetAllSessions() []*common.SessionInfo {
	return bs.mem.GetAllSessions()
}

func (bs *BoltStore) SessionCount() int {
	return bs.mem.SessionCount()
}

func (bs *BoltStore) RecordComplaint(nodeID string) {
	bs.mem.RecordComplaint(nodeID)
}

func (bs *BoltStore) isCooling(nodeID string) bool {
	return bs.mem.isCooling(nodeID)
}
