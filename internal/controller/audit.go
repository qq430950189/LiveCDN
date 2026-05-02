package controller

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	bolt "go.etcd.io/bbolt"
)

// 审计日志 bucket
var bucketAuditLog = []byte("audit_log")

// AuditAction 审计操作类型
type AuditAction string

const (
	AuditNodeRegister   AuditAction = "node_register"    // 节点注册
	AuditNodeApprove    AuditAction = "node_approve"     // 节点审批
	AuditNodeRemove     AuditAction = "node_remove"      // 节点移除
	AuditNodeStatusSet  AuditAction = "node_status_set"  // 节点状态变更
	AuditStreamStart    AuditAction = "stream_start"     // 开播
	AuditStreamStop     AuditAction = "stream_stop"      // 停播
	AuditKeyRotate      AuditAction = "key_rotate"       // 密钥轮换
	AuditTokenSign      AuditAction = "token_sign"       // Token 签发
	AuditDomainAdd      AuditAction = "domain_add"       // 域名添加
	AuditDomainRemove   AuditAction = "domain_remove"    // 域名移除
	AuditDomainSwitch   AuditAction = "domain_switch"    // 域名切换
	AuditConfigUpdate   AuditAction = "config_update"    // 配置热更新
	AuditLatencyModeSet AuditAction = "latency_mode_set" // 延迟档位变更
	AuditDispatch       AuditAction = "dispatch"         // 调度分配
	AuditPublishAuth    AuditAction = "publish_auth"     // 推流鉴权
)

// AuditEntry 审计日志条目
type AuditEntry struct {
	ID        string      `json:"id"`         // 唯一 ID (时间戳+序号)
	Action    AuditAction `json:"action"`     // 操作类型
	Actor     string      `json:"actor"`      // 操作者 (admin / agent / system / player)
	Target    string      `json:"target"`     // 操作对象 (node_id / stream_key / domain)
	Detail    string      `json:"detail"`     // 详细描述
	Metadata  interface{} `json:"metadata"`   // 附加数据 (可选)
	Timestamp time.Time   `json:"timestamp"`  // 发生时间
	Signature string      `json:"signature"`  // HMAC-SHA256 签名 (防篡改)
}

// AuditLogger 审计日志记录器
// 所有管理员操作、节点准入、Token 签发、密钥轮换都记录到 bbolt
// 每条记录带 HMAC-SHA256 签名, 防止日志被篡改
type AuditLogger struct {
	mu      sync.Mutex
	db      *bolt.DB
	signKey []byte // HMAC 签名密钥 (admin_token)
	counter uint64 // 自增序号
}

// NewAuditLogger 创建审计日志记录器
func NewAuditLogger(db *bolt.DB, adminToken string) *AuditLogger {
	al := &AuditLogger{
		db:      db,
		signKey: []byte(adminToken),
		counter: uint64(time.Now().Unix()),
	}

	// 确保 bucket 存在
	if db != nil {
		db.Update(func(tx *bolt.Tx) error {
			_, err := tx.CreateBucketIfNotExists(bucketAuditLog)
			return err
		})
	}

	return al
}

// Log 记录一条审计日志
func (al *AuditLogger) Log(action AuditAction, actor, target, detail string, metadata interface{}) {
	al.mu.Lock()
	defer al.mu.Unlock()

	al.counter++
	entry := AuditEntry{
		ID:        fmt.Sprintf("%d-%d", time.Now().UnixMilli(), al.counter),
		Action:    action,
		Actor:     actor,
		Target:    target,
		Detail:    detail,
		Metadata:  metadata,
		Timestamp: time.Now(),
	}

	// 生成 HMAC-SHA256 签名 (防篡改)
	entry.Signature = al.signEntry(entry)

	// 结构化日志输出
	log.Info().
		Str("audit_id", entry.ID).
		Str("action", string(entry.Action)).
		Str("actor", entry.Actor).
		Str("target", truncateStr(target, 16)).
		Str("detail", entry.Detail).
		Msg("AUDIT")

	// 持久化到 bbolt
	if al.db != nil {
		al.persist(entry)
	}
}

// signEntry 对审计条目生成 HMAC-SHA256 签名
// 签名覆盖: id + action + actor + target + detail + timestamp
// 任何字段被篡改, 签名验证失败
func (al *AuditLogger) signEntry(entry AuditEntry) string {
	mac := hmac.New(sha256.New, al.signKey)
	mac.Write([]byte(entry.ID))
	mac.Write([]byte(entry.Action))
	mac.Write([]byte(entry.Actor))
	mac.Write([]byte(entry.Target))
	mac.Write([]byte(entry.Detail))
	mac.Write([]byte(entry.Timestamp.Format(time.RFC3339Nano)))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyEntry 验证审计条目签名 (防篡改检测)
func (al *AuditLogger) VerifyEntry(entry AuditEntry) bool {
	expected := al.signEntry(entry)
	return hmac.Equal([]byte(entry.Signature), []byte(expected))
}

// persist 持久化到 bbolt
func (al *AuditLogger) persist(entry AuditEntry) {
	data, err := json.Marshal(entry)
	if err != nil {
		log.Error().Err(err).Str("audit_id", entry.ID).Msg("failed to marshal audit entry")
		return
	}

	err = al.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAuditLog)
		if b == nil {
			return fmt.Errorf("audit_log bucket not found")
		}
		return b.Put([]byte(entry.ID), data)
	})

	if err != nil {
		log.Error().Err(err).Str("audit_id", entry.ID).Msg("failed to persist audit entry")
	}
}

// Query 查询审计日志
// action: 过滤操作类型 (空字符串 = 全部)
// limit: 最大返回条数
// 返回按时间倒序 (最新在前)
func (al *AuditLogger) Query(action AuditAction, limit int) []AuditEntry {
	if al.db == nil {
		return nil
	}

	var entries []AuditEntry
	al.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAuditLog)
		if b == nil {
			return nil
		}

		cursor := b.Cursor()
		// 从后往前遍历 (bbolt 按 key 字典序, ID 包含时间戳所以有序)
		for k, v := cursor.Last(); k != nil; k, v = cursor.Prev() {
			var entry AuditEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				continue
			}

			// 过滤
			if action != "" && entry.Action != action {
				continue
			}

			entries = append(entries, entry)
			if len(entries) >= limit {
				break
			}
		}
		return nil
	})

	return entries
}

// Count 统计审计日志条数
func (al *AuditLogger) Count() int {
	if al.db == nil {
		return 0
	}

	count := 0
	al.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAuditLog)
		if b == nil {
			return nil
		}
		count = b.Stats().KeyN
		return nil
	})
	return count
}

// VerifyIntegrity 验证所有审计日志的完整性 (防篡改扫描)
// 返回被篡改的条目数
func (al *AuditLogger) VerifyIntegrity() int {
	if al.db == nil {
		return 0
	}

	tampered := 0
	al.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAuditLog)
		if b == nil {
			return nil
		}

		checked := 0
		b.ForEach(func(k, v []byte) error {
			var entry AuditEntry
			if err := json.Unmarshal(v, &entry); err != nil {
				tampered++ // 无法解析, 篡改嫌疑
				return nil
			}
			if !al.VerifyEntry(entry) {
				tampered++
			}
			checked++
			return nil
		})
		log.Info().Int("checked", checked).Int("tampered", tampered).Msg("audit integrity check")
		return nil
	})

	return tampered
}

// truncateStr 截断字符串
func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
