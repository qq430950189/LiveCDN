package controller

import (
	"testing"
	"time"
)

func TestAuditLogSignAndVerify(t *testing.T) {
	al := NewAuditLogger(nil, "test-admin-token")

	entry := AuditEntry{
		ID:        "1234567890-1",
		Action:    AuditNodeApprove,
		Actor:     "admin",
		Target:    "node-1",
		Detail:    "approved to online",
		Timestamp: time.Now(),
	}

	// 签名
	entry.Signature = al.signEntry(entry)

	// 验证通过
	if !al.VerifyEntry(entry) {
		t.Error("expected entry to be valid")
	}

	// 篡改后验证失败
	entry.Detail = "approved to offline"
	if al.VerifyEntry(entry) {
		t.Error("expected entry to be invalid after tampering")
	}

	// 篡改 action 后验证失败
	entry.Detail = "approved to online"
	entry.Action = AuditNodeRemove
	if al.VerifyEntry(entry) {
		t.Error("expected entry to be invalid after tampering action")
	}
}

func TestAuditLogMultipleActions(t *testing.T) {
	al := NewAuditLogger(nil, "test-admin-token")

	// 记录多条日志 (不持久化, 仅验证签名)
	actions := []AuditAction{
		AuditNodeRegister,
		AuditNodeApprove,
		AuditStreamStart,
		AuditKeyRotate,
		AuditDomainSwitch,
		AuditConfigUpdate,
	}

	for _, action := range actions {
		al.counter++
		entry := AuditEntry{
			ID:        "test-entry",
			Action:    action,
			Actor:     "admin",
			Target:    "test-target",
			Detail:    "test detail",
			Timestamp: time.Now(),
		}
		entry.Signature = al.signEntry(entry)

		if !al.VerifyEntry(entry) {
			t.Errorf("entry with action %s should be valid", action)
		}
	}
}

func TestAuditLogDifferentKeys(t *testing.T) {
	al1 := NewAuditLogger(nil, "key-1")
	al2 := NewAuditLogger(nil, "key-2")

	entry := AuditEntry{
		ID:        "test-entry",
		Action:    AuditStreamStart,
		Actor:     "admin",
		Target:    "stream-1",
		Detail:    "stream started",
		Timestamp: time.Now(),
	}

	entry.Signature = al1.signEntry(entry)

	// 同一 key 验证通过
	if !al1.VerifyEntry(entry) {
		t.Error("expected valid with same key")
	}

	// 不同 key 验证失败
	if al2.VerifyEntry(entry) {
		t.Error("expected invalid with different key")
	}
}

func TestAuditLogCounterIncrement(t *testing.T) {
	al := NewAuditLogger(nil, "test-key")

	initialCounter := al.counter

	// Log 会增加 counter
	al.Log(AuditNodeRegister, "agent", "node-1", "registered", nil)
	if al.counter != initialCounter+1 {
		t.Errorf("expected counter %d, got %d", initialCounter+1, al.counter)
	}

	al.Log(AuditNodeApprove, "admin", "node-1", "approved", nil)
	if al.counter != initialCounter+2 {
		t.Errorf("expected counter %d, got %d", initialCounter+2, al.counter)
	}
}

func TestAuditLogQueryNoDB(t *testing.T) {
	al := NewAuditLogger(nil, "test-key")

	// 没有 bbolt DB, 查询返回 nil
	entries := al.Query("", 10)
	if entries != nil {
		t.Error("expected nil when no db")
	}

	count := al.Count()
	if count != 0 {
		t.Error("expected 0 count when no db")
	}

	tampered := al.VerifyIntegrity()
	if tampered != 0 {
		t.Error("expected 0 tampered when no db")
	}
}
