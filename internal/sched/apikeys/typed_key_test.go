package apikeys

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	mgr, err := NewManager(dir+"/keys.json", dir+"/audit.jsonl")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr
}

func TestCreateTypedKeyFormatAndPerms(t *testing.T) {
	mgr := newTestManager(t)

	key, raw, err := mgr.CreateTypedKey("ci", KeyTypeTask, nil, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("CreateTypedKey: %v", err)
	}
	if !strings.HasPrefix(raw, "fleet_task_") {
		t.Fatalf("raw key %q does not have fleet_task_ prefix", raw)
	}
	kt, _, perr := ParseAPIKey(raw)
	if perr != nil || kt != KeyTypeTask {
		t.Fatalf("ParseAPIKey(%q) = (%q, %v), want task", raw, kt, perr)
	}
	if key.Type != KeyTypeTask {
		t.Errorf("stored Type = %q, want task", key.Type)
	}
	if !key.HasPermission(models.PermissionCreateTask) {
		t.Error("task key should have CreateTask permission")
	}
	if key.HasPermission(models.PermissionAdmin) {
		t.Error("task key must not have Admin permission")
	}

	// The raw key validates and resolves back to the same stored key.
	valid, got, _ := mgr.ValidateKey(raw, nil, nil, nil, nil)
	if !valid || got == nil || got.KeyID != key.KeyID {
		t.Fatalf("ValidateKey did not resolve the typed key")
	}
}

func TestCreateTypedKeyWebhookSlugScope(t *testing.T) {
	mgr := newTestManager(t)

	key, _, err := mgr.CreateTypedKey("gh", KeyTypeWebhook, []string{"pr-review", "deploy-staging"}, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("CreateTypedKey webhook: %v", err)
	}
	if !key.AllowsTriggerSlug("pr-review") || !key.AllowsTriggerSlug("deploy-staging") {
		t.Error("webhook key should allow its configured slugs")
	}
	if key.AllowsTriggerSlug("prod-deploy") {
		t.Error("webhook key must not allow an unconfigured slug")
	}
	if len(KeyTypeWebhook.Permissions()) != 0 || key.HasPermission(models.PermissionCreateTask) {
		t.Error("webhook key must carry no task permissions")
	}

	// A non-webhook type must not retain trigger slugs even if supplied.
	ro, _, err := mgr.CreateTypedKey("ro", KeyTypeReadonly, []string{"pr-review"}, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("CreateTypedKey readonly: %v", err)
	}
	if len(ro.AllowedTriggerSlugs) != 0 {
		t.Errorf("non-webhook key retained trigger slugs: %v", ro.AllowedTriggerSlugs)
	}
}

func TestCreateTypedKeyRejectsBadType(t *testing.T) {
	mgr := newTestManager(t)
	if _, _, err := mgr.CreateTypedKey("x", KeyType("superuser"), nil, nil, 0, nil, ""); err == nil {
		t.Fatal("CreateTypedKey should reject an invalid type")
	}
}

func TestLookupKeyType(t *testing.T) {
	mgr := newTestManager(t)

	_, taskRaw, _ := mgr.CreateTypedKey("t", KeyTypeTask, nil, nil, 0, nil, "")
	_, roRaw, _ := mgr.CreateTypedKey("r", KeyTypeReadonly, nil, nil, 0, nil, "")
	// Legacy untyped key.
	_, skRaw, _ := mgr.CreateKey("legacy", nil, nil, nil, 0, nil, "")

	if kt, hasCreate, ok := mgr.LookupKeyType(taskRaw); !ok || kt != KeyTypeTask || !hasCreate {
		t.Errorf("LookupKeyType(task) = (%q,%v,%v)", kt, hasCreate, ok)
	}
	if kt, hasCreate, ok := mgr.LookupKeyType(roRaw); !ok || kt != KeyTypeReadonly || hasCreate {
		t.Errorf("LookupKeyType(readonly) = (%q,%v,%v); want readonly without create", kt, hasCreate, ok)
	}
	if kt, _, ok := mgr.LookupKeyType(skRaw); !ok || kt != KeyTypeLegacy {
		t.Errorf("LookupKeyType(legacy) = (%q,_,%v); want legacy", kt, ok)
	}
	if _, _, ok := mgr.LookupKeyType("fleet_task_doesnotexist"); ok {
		t.Error("LookupKeyType should return ok=false for an unknown key")
	}
}

func TestRotatePreservesType(t *testing.T) {
	mgr := newTestManager(t)
	key, _, err := mgr.CreateTypedKey("svc", KeyTypeReadonly, nil, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("CreateTypedKey: %v", err)
	}
	rotated, newRaw, err := mgr.RotateKey(key.KeyID, 1)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if rotated.Type != KeyTypeReadonly {
		t.Errorf("rotated key type = %q, want readonly", rotated.Type)
	}
	if !strings.HasPrefix(newRaw, "fleet_readonly_") {
		t.Errorf("rotated raw key %q lost its typed prefix", newRaw)
	}
}

func TestTypedKeyPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	mgr, err := NewManager(dir+"/keys.json", dir+"/audit.jsonl")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	key, _, err := mgr.CreateTypedKey("wh", KeyTypeWebhook, []string{"pr-review"}, nil, 0, nil, "")
	if err != nil {
		t.Fatalf("CreateTypedKey: %v", err)
	}

	// Reload from disk: type + slugs must round-trip through the JSON store.
	mgr2, err := NewManager(dir+"/keys.json", dir+"/audit.jsonl")
	if err != nil {
		t.Fatalf("reload NewManager: %v", err)
	}
	got := mgr2.GetKey(key.KeyID)
	if got == nil {
		t.Fatal("key not found after reload")
	}
	if got.Type != KeyTypeWebhook || !got.AllowsTriggerSlug("pr-review") {
		t.Errorf("after reload type=%q slugs=%v", got.Type, got.AllowedTriggerSlugs)
	}
}
