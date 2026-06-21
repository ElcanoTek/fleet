package apikeys

import (
	"os"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestAPIKeyManager(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "apikeys_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	storagePath := tmpDir + "/keys.json"
	auditLogPath := tmpDir + "/audit.jsonl"

	manager, err := NewManager(storagePath, auditLogPath)
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	// 1. Create Key
	key, rawKey, err := manager.CreateKey(
		"test-key",
		[]string{"*"},
		[]models.Permission{models.PermissionViewTasks}, // Changed from PermissionReadTasks
		nil,
		100,
		nil,
		"Test Key",
	)
	if err != nil {
		t.Fatalf("Failed to create key: %v", err)
	}

	if key.Name != "test-key" {
		t.Errorf("Expected name test-key, got %s", key.Name)
	}
	if rawKey == "" {
		t.Error("Returned raw key is empty")
	}

	// 2. Validate Key
	valid, validatedKey, msg := manager.ValidateKey(rawKey, nil, nil, nil, nil)
	if !valid {
		t.Errorf("Key validation failed: %s", msg)
	}
	if validatedKey.KeyID != key.KeyID {
		t.Errorf("Expected key ID %s, got %s", key.KeyID, validatedKey.KeyID)
	}

	// Validate Permission
	perm := models.PermissionViewTasks // Changed from PermissionReadTasks
	valid, _, _ = manager.ValidateKey(rawKey, &perm, nil, nil, nil)
	if !valid {
		t.Error("Permission check failed")
	}

	missingPerm := models.PermissionAdmin
	valid, _, _ = manager.ValidateKey(rawKey, &missingPerm, nil, nil, nil)
	if valid {
		t.Error("Permission check should have failed")
	}

	// 3. Rotate Key
	oldRawKey := rawKey
	updatedKey, newRawKey, err := manager.RotateKey(key.KeyID, 1)
	if err != nil {
		t.Fatalf("Failed to rotate key: %v", err)
	}

	if newRawKey == oldRawKey {
		t.Error("Rotated key is same as old key")
	}

	// Old key should still be valid (grace period)
	valid, _, _ = manager.ValidateKey(oldRawKey, nil, nil, nil, nil)
	if !valid {
		t.Error("Old key should be valid during grace period")
	}

	// New key should be valid
	valid, _, _ = manager.ValidateKey(newRawKey, nil, nil, nil, nil)
	if !valid {
		t.Error("New key should be valid")
	}

	// 4. Rate Limiting
	// Reset limits for test simplicity if needed, but we created a fresh key
	// We set limit to 100, let's create a key with limit 2
	// Assigned to _ to avoid unused variable error
	_, rawKey2, _ := manager.CreateKey("limit-test", nil, nil, nil, 2, nil, "")

	valid, _, _ = manager.ValidateKey(rawKey2, nil, nil, nil, nil) // 1
	if !valid {
		t.Error("Should be valid")
	}
	valid, _, _ = manager.ValidateKey(rawKey2, nil, nil, nil, nil) // 2
	if !valid {
		t.Error("Should be valid")
	}
	valid, _, _ = manager.ValidateKey(rawKey2, nil, nil, nil, nil) // 3 (Exceeded)
	if valid {
		t.Error("Should be rate limited")
	}

	// 5. Revoke Key
	if err := manager.RevokeKey(updatedKey.KeyID); err != nil {
		t.Fatalf("Failed to revoke key: %v", err)
	}

	valid, _, _ = manager.ValidateKey(newRawKey, nil, nil, nil, nil)
	if valid {
		t.Error("Revoked key should not be valid")
	}

	// 6. Delete Key
	if err := manager.DeleteKey(updatedKey.KeyID); err != nil {
		t.Fatalf("Failed to delete key: %v", err)
	}

	if manager.GetKey(updatedKey.KeyID) != nil {
		t.Error("Key should be deleted")
	}
}

func TestGetAuditLog(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "apikeys_audit_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	storagePath := tmpDir + "/keys.json"
	auditLogPath := tmpDir + "/audit.jsonl"

	manager, err := NewManager(storagePath, auditLogPath)
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	// Helper to ptr
	ptr := func(s string) *string { return &s }

	// Test Case 6: Missing file
	logs := manager.GetAuditLog(nil, nil, nil, 0)
	if len(logs) != 0 {
		t.Errorf("Expected empty logs for missing file, got %d", len(logs))
	}

	// Write some test logs directly to file to ensure we know exact timestamps
	f, err := os.OpenFile(auditLogPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("Failed to create audit log file: %v", err)
	}

	logEntries := []string{
		`{"key_id":"key1","action":"validate","timestamp":"2023-01-01T10:00:00Z","ip":"127.0.0.1"}`,
		`{"key_id":"key2","action":"create","timestamp":"2023-01-01T11:00:00Z"}`,
		`{"key_id":"key1","action":"validate","timestamp":"2023-01-01T12:00:00Z","ip":"192.168.1.1"}`,
		`{"key_id":"key3","action":"rotate","timestamp":"2023-01-01T13:00:00Z"}`,
		`{"key_id":"key1","action":"revoke","timestamp":"2023-01-01T14:00:00Z"}`,
	}

	for _, entry := range logEntries {
		f.WriteString(entry + "\n")
	}
	f.Close()

	// Test Case 1: Fetch all logs without filters (Reverse order)
	logs = manager.GetAuditLog(nil, nil, nil, 0)
	if len(logs) != 5 {
		t.Errorf("Expected 5 logs, got %d", len(logs))
	} else if logs[0]["key_id"] != "key1" || logs[0]["action"] != "revoke" {
		t.Errorf("Expected most recent log first (key1/revoke), got %v", logs[0])
	}

	// Test Case 2: Filter by limit
	logs = manager.GetAuditLog(nil, nil, nil, 2)
	if len(logs) != 2 {
		t.Errorf("Expected 2 logs, got %d", len(logs))
	} else if logs[0]["action"] != "revoke" || logs[1]["action"] != "rotate" {
		t.Errorf("Expected recent 2 logs, got %v and %v", logs[0], logs[1])
	}

	// Test Case 3: Filter by keyID
	logs = manager.GetAuditLog(ptr("key1"), nil, nil, 0)
	if len(logs) != 3 {
		t.Errorf("Expected 3 logs for key1, got %d", len(logs))
	} else {
		for _, log := range logs {
			if log["key_id"] != "key1" {
				t.Errorf("Expected only key1, got %v", log["key_id"])
			}
		}
	}

	// Test Case 4: Filter by action
	logs = manager.GetAuditLog(nil, ptr("validate"), nil, 0)
	if len(logs) != 2 {
		t.Errorf("Expected 2 logs for validate, got %d", len(logs))
	} else {
		for _, log := range logs {
			if log["action"] != "validate" {
				t.Errorf("Expected only validate action, got %v", log["action"])
			}
		}
	}

	// Test Case 5: Filter by since timestamp
	// since 2023-01-01T12:30:00Z
	// Should return 13:00:00Z and 14:00:00Z
	tSince, _ := time.Parse(time.RFC3339, "2023-01-01T12:30:00Z")
	logs = manager.GetAuditLog(nil, nil, &tSince, 0)
	if len(logs) != 2 {
		t.Errorf("Expected 2 logs since 12:30, got %d", len(logs))
	} else if logs[0]["action"] != "revoke" || logs[1]["action"] != "rotate" {
		t.Errorf("Expected revoke and rotate, got %v and %v", logs[0], logs[1])
	}

	// Combined filters
	logs = manager.GetAuditLog(ptr("key1"), ptr("validate"), nil, 0)
	if len(logs) != 2 {
		t.Errorf("Expected 2 logs for key1/validate, got %d", len(logs))
	}
}
