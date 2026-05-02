package controller

import (
	"fmt"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/serialx/hashring"
	"github.com/user/live-cdn/internal/common"
)

// ConsistentHashScheduler 基于一致性哈希的调度器
//
// 核心优势：
//   - 新增/删除节点时，只有 1/N 的观众会被重新分配
//   - 避免扩缩容时的连接风暴
//   - 每个物理节点配 virtualReplicas 个虚拟节点，确保均匀分布
//
// 调度流程：
//  1. 从 healthy 节点构建 hash ring
//  2. 用 clientIP 作为 key 映射到主节点
//  3. 顺时针找后续节点作为备节点
//  4. 如果哈希节点不健康，fallback 到加权随机
type ConsistentHashScheduler struct {
	store           *MemoryStore
	virtualReplicas int           // 每个物理节点的虚拟节点数
	fallback        *Scheduler   // fallback 到加权随机
}

func NewConsistentHashScheduler(store *MemoryStore) *ConsistentHashScheduler {
	return &ConsistentHashScheduler{
		store:           store,
		virtualReplicas: 50,
		fallback:        NewScheduler(store),
	}
}

// Dispatch 使用一致性哈希选择节点
func (chs *ConsistentHashScheduler) Dispatch(streamKey, clientIP, clientRegion, clientISP string) (*DispatchResult, error) {
	healthyNodes := chs.store.GetHealthyNodes()
	if len(healthyNodes) == 0 {
		return nil, ErrNoAvailableNodes
	}

	// 只有 1 个节点时直接返回
	if len(healthyNodes) == 1 {
		node := healthyNodes[0]
		ep := common.NodeEndpoint{
			URL:             buildNodeURL(node),
			FLVURL:          buildNodeFLVURL(node),
			HLSURL:          buildNodeHLSURL(node),
			Domain:          node.Domain,
			Port:            node.Port,
			Protocol:        node.Protocol,
			WsPath:          node.WsPath,
			Region:          node.Region,
			ISP:             node.ISP,
			Priority:        0,
			CascadeRelayURL: buildCascadeRelayURL(node),
			IPv6FLVURL:      buildIPv6FLVURL(node),
		}
		return &DispatchResult{Nodes: []common.NodeEndpoint{ep}}, nil
	}

	// 构建一致性哈希环 (添加虚拟节点以提高均匀性)
	nodeIDs := make([]string, 0, len(healthyNodes)*3)
	nodeMap := make(map[string]*common.NodeInfo, len(healthyNodes))
	for _, n := range healthyNodes {
		nodeMap[n.NodeID] = n
		// 每个物理节点添加虚拟节点 (格式: nodeID__vn)
		// GetNode 返回时需要提取物理节点 ID
		for vn := 0; vn < chs.virtualReplicas; vn++ {
			nodeIDs = append(nodeIDs, fmt.Sprintf("%s__%d", n.NodeID, vn))
		}
	}

	ring := hashring.New(nodeIDs)

	// 用 clientIP 映射主节点
	virtualPrimaryID, ok := ring.GetNode(clientIP)
	if !ok {
		// fallback 到加权随机
		log.Warn().Str("client_ip", clientIP).Msg("consistent hash miss, falling back to weighted random")
		return chs.fallback.Dispatch(streamKey, clientIP, clientRegion, clientISP)
	}
	primaryID := extractPhysicalID(virtualPrimaryID)

	// 收集备节点 (顺时针方向取 2 个不同的节点)
	backupIDs := make([]string, 0, 2)
	for i := 0; i < len(healthyNodes) && len(backupIDs) < 2; i++ {
		key := clientIP + string(rune(i+'A')) // 不同的 key 映射到不同节点
		if virtualID, ok := ring.GetNode(key); ok {
			id := extractPhysicalID(virtualID)
			if id != primaryID {
				alreadyAdded := false
				for _, bid := range backupIDs {
					if bid == id {
						alreadyAdded = true
						break
					}
				}
				if !alreadyAdded {
					backupIDs = append(backupIDs, id)
				}
			}
		}
	}

	// 构建 endpoints
	endpoints := make([]common.NodeEndpoint, 0, 1+len(backupIDs))

	if primary, ok := nodeMap[primaryID]; ok {
		ep := common.NodeEndpoint{
			URL:             buildNodeURL(primary),
			FLVURL:          buildNodeFLVURL(primary),
			HLSURL:          buildNodeHLSURL(primary),
			Domain:          primary.Domain,
			Port:            primary.Port,
			Protocol:        primary.Protocol,
			WsPath:          primary.WsPath,
			Region:          primary.Region,
			ISP:             primary.ISP,
			Priority:        0,
			CascadeRelayURL: buildCascadeRelayURL(primary),
			IPv6FLVURL:      buildIPv6FLVURL(primary),
		}
		endpoints = append(endpoints, ep)
	}

	for i, bid := range backupIDs {
		if backup, ok := nodeMap[bid]; ok {
			ep := common.NodeEndpoint{
				URL:             buildNodeURL(backup),
				FLVURL:          buildNodeFLVURL(backup),
				HLSURL:          buildNodeHLSURL(backup),
				Domain:          backup.Domain,
				Port:            backup.Port,
				Protocol:        backup.Protocol,
				WsPath:          backup.WsPath,
				Region:          backup.Region,
				ISP:             backup.ISP,
				Priority:        i + 1,
				CascadeRelayURL: buildCascadeRelayURL(backup),
				IPv6FLVURL:      buildIPv6FLVURL(backup),
			}
			endpoints = append(endpoints, ep)
		}
	}

	if len(endpoints) == 0 {
		return nil, ErrNoAvailableNodes
	}

	return &DispatchResult{Nodes: endpoints}, nil
}

// extractPhysicalID 从虚拟节点 ID 提取物理节点 ID
// 虚拟节点格式: "nodeID__vn" → 提取 "nodeID"
func extractPhysicalID(virtualID string) string {
	if idx := strings.LastIndex(virtualID, "__"); idx >= 0 {
		return virtualID[:idx]
	}
	return virtualID
}
