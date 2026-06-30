package apikeys

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestParseAPIKey(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantKT  KeyType
		wantErr bool
	}{
		{"admin", "fleet_admin_abcDEF123", KeyTypeAdmin, false},
		{"task", "fleet_task_abcDEF123", KeyTypeTask, false},
		{"webhook", "fleet_webhook_abcDEF123", KeyTypeWebhook, false},
		{"readonly", "fleet_readonly_abcDEF123", KeyTypeReadonly, false},
		{"legacy sk-", "sk-AAAABBBBCCCC", KeyTypeLegacy, false},
		{"empty", "", "", true},
		{"bad prefix", "kortix_admin_abc", "", true},
		{"no type segment", "fleet_abcDEF123", "", true},
		{"unknown type", "fleet_superuser_abcDEF123", "", true},
		{"empty suffix", "fleet_admin_", "", true},
		{"non-base58 suffix (ambiguous chars)", "fleet_task_0OIl", "", true},
		{"suffix with underscore (not base58)", "fleet_task_abc_def", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kt, suffix, err := ParseAPIKey(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseAPIKey(%q) = (%q,%q,nil), want error", tt.raw, kt, suffix)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAPIKey(%q) unexpected error: %v", tt.raw, err)
			}
			if kt != tt.wantKT {
				t.Errorf("ParseAPIKey(%q) type = %q, want %q", tt.raw, kt, tt.wantKT)
			}
		})
	}
}

func TestKeyTypeValid(t *testing.T) {
	for _, kt := range []KeyType{KeyTypeAdmin, KeyTypeTask, KeyTypeWebhook, KeyTypeReadonly} {
		if !kt.Valid() {
			t.Errorf("KeyType(%q).Valid() = false, want true", kt)
		}
	}
	for _, kt := range []KeyType{KeyTypeLegacy, "superuser", "Admin", "TASK"} {
		if kt.Valid() {
			t.Errorf("KeyType(%q).Valid() = true, want false", kt)
		}
	}
}

func TestKeyTypePermissions(t *testing.T) {
	has := func(perms []models.Permission, want models.Permission) bool {
		for _, p := range perms {
			if p == want {
				return true
			}
		}
		return false
	}

	if !has(KeyTypeAdmin.Permissions(), models.PermissionAdmin) {
		t.Error("admin type should carry PermissionAdmin")
	}
	taskPerms := KeyTypeTask.Permissions()
	if !has(taskPerms, models.PermissionCreateTask) || !has(taskPerms, models.PermissionViewTasks) || !has(taskPerms, models.PermissionViewLogs) {
		t.Errorf("task type perms = %v, want create/view/logs", taskPerms)
	}
	if has(taskPerms, models.PermissionAdmin) {
		t.Error("task type must not carry PermissionAdmin")
	}
	roPerms := KeyTypeReadonly.Permissions()
	if has(roPerms, models.PermissionCreateTask) {
		t.Error("readonly type must not carry PermissionCreateTask")
	}
	if !has(roPerms, models.PermissionViewTasks) {
		t.Error("readonly type should carry PermissionViewTasks")
	}
	if len(KeyTypeWebhook.Permissions()) != 0 {
		t.Errorf("webhook type perms = %v, want empty", KeyTypeWebhook.Permissions())
	}
}

func TestTypeAllowsMethod(t *testing.T) {
	cases := []struct {
		kt     KeyType
		method string
		want   bool
	}{
		{KeyTypeAdmin, http.MethodGet, true},
		{KeyTypeAdmin, http.MethodDelete, true},
		{KeyTypeTask, http.MethodGet, true},
		{KeyTypeTask, http.MethodPost, true},
		{KeyTypeTask, http.MethodDelete, true},
		{KeyTypeReadonly, http.MethodGet, true},
		{KeyTypeReadonly, http.MethodHead, true},
		{KeyTypeReadonly, http.MethodPost, false},
		{KeyTypeReadonly, http.MethodPut, false},
		{KeyTypeReadonly, http.MethodDelete, false},
		{KeyTypeWebhook, http.MethodGet, false},
		{KeyTypeWebhook, http.MethodPost, false},
		{KeyTypeLegacy, http.MethodGet, false}, // legacy is handled by the caller, not this gate
	}
	for _, c := range cases {
		if got := TypeAllowsMethod(c.kt, c.method); got != c.want {
			t.Errorf("TypeAllowsMethod(%q, %s) = %v, want %v", c.kt, c.method, got, c.want)
		}
	}
}

func TestRandomBase58(t *testing.T) {
	const n = 32
	seen := make(map[string]struct{})
	for i := 0; i < 50; i++ {
		s, err := randomBase58(n)
		if err != nil {
			t.Fatalf("randomBase58 error: %v", err)
		}
		if len(s) != n {
			t.Fatalf("randomBase58(%d) length = %d", n, len(s))
		}
		if !isBase58(s) {
			t.Fatalf("randomBase58 produced non-base58 string %q", s)
		}
		if strings.ContainsAny(s, "0OIl+/") {
			t.Fatalf("randomBase58 produced an ambiguous char in %q", s)
		}
		if _, dup := seen[s]; dup {
			t.Fatalf("randomBase58 produced a duplicate: %q", s)
		}
		seen[s] = struct{}{}
	}
}

func TestIsBase58(t *testing.T) {
	if !isBase58("abcDEF123") {
		t.Error("isBase58 should accept a valid base58 string")
	}
	for _, bad := range []string{"", "0abc", "Oabc", "Iabc", "labc", "a+b", "a/b"} {
		if isBase58(bad) {
			t.Errorf("isBase58(%q) = true, want false", bad)
		}
	}
}
