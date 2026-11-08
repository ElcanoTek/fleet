package httpapi

import (
	"errors"
	"net/http"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

func TestExternalAccessProvisioningDefaultsMemberAndIgnoresStaleVersions(t *testing.T) {
	t.Parallel()

	s := memberFixture(t)
	h := s.Routes()
	const email = "alice@example.com"

	grant := map[string]any{
		"event_id": "grant-2", "issuer": "https://auth.example.com",
		"subject": "account-123", "action": "grant", "version": 2,
		"issued_at": 1_000,
		"settings":  map[string]string{"chat_role": "member", "ops_role": "none"},
	}
	if w := do(t, h, http.MethodPost, "/auth/external-access", grant, email); w.Code != http.StatusNoContent {
		t.Fatalf("grant: status %d body %q", w.Code, w.Body.String())
	}
	if user, err := s.concreteStore(t).GetUser(t.Context(), email); err != nil || user.Role != store.RoleMember {
		t.Fatalf("granted user = (%+v, %v), want member", user, err)
	}

	revoke := map[string]any{
		"event_id": "revoke-3", "issuer": "https://auth.example.com",
		"subject": "account-123", "action": "revoke", "version": 3,
		"issued_at": 1_001,
		"settings":  map[string]string{"chat_role": "member", "ops_role": "none"},
	}
	if w := do(t, h, http.MethodPost, "/auth/external-access", revoke, email); w.Code != http.StatusNoContent {
		t.Fatalf("revoke: status %d body %q", w.Code, w.Body.String())
	}
	if _, err := s.concreteStore(t).GetUser(t.Context(), email); !errors.Is(err, store.ErrUserNotFound) {
		t.Fatalf("revoked user remains enabled: %v", err)
	}

	if w := do(t, h, http.MethodPost, "/auth/external-access", grant, email); w.Code != http.StatusNoContent {
		t.Fatalf("stale grant: status %d body %q", w.Code, w.Body.String())
	}
	if _, err := s.concreteStore(t).GetUser(t.Context(), email); !errors.Is(err, store.ErrUserNotFound) {
		t.Fatalf("stale grant re-enabled account: %v", err)
	}

	grant["event_id"], grant["version"] = "grant-4", 4
	if w := do(t, h, http.MethodPost, "/auth/external-access", grant, email); w.Code != http.StatusNoContent {
		t.Fatalf("regrant: status %d body %q", w.Code, w.Body.String())
	}
	if user, err := s.concreteStore(t).GetUser(t.Context(), email); err != nil || user.Role != store.RoleMember {
		t.Fatalf("regranted user = (%+v, %v), want member", user, err)
	}
}

func TestExternalAccessProvisioningRejectsSplitAdminPermissions(t *testing.T) {
	t.Parallel()

	s := memberFixture(t)
	h := s.Routes()
	body := map[string]any{
		"event_id": "grant-1", "issuer": "https://auth.example.com",
		"subject": "account-123", "action": "grant", "version": 1,
		"issued_at": 1_000,
		"settings":  map[string]string{"chat_role": "admin", "ops_role": "none"},
	}

	if w := do(t, h, http.MethodPost, "/auth/external-access", body, "alice@example.com"); w.Code != http.StatusBadRequest {
		t.Fatalf("split admin: status %d want 400 (body %q)", w.Code, w.Body.String())
	}
}

func TestExternalAccessProvisioningAppliesSelectedChatAndOpsRoles(t *testing.T) {
	t.Parallel()

	s := memberFixture(t)
	h := s.Routes()
	const email = "roles@example.com"

	cases := []struct {
		version           int
		chatRole, opsRole string
	}{
		{1, store.RoleViewer, "readonly"},
		{2, store.RoleMember, "client"},
		{3, store.RoleAdmin, "admin"},
	}
	for _, tc := range cases {
		body := map[string]any{
			"event_id": "grant-role", "issuer": "https://auth.example.com",
			"subject": "roles-account", "action": "grant", "version": tc.version,
			"issued_at": int64(1_000 + tc.version),
			"settings":  map[string]string{"chat_role": tc.chatRole, "ops_role": tc.opsRole},
		}
		if w := do(t, h, http.MethodPost, "/auth/external-access", body, email); w.Code != http.StatusNoContent {
			t.Fatalf("version %d: status %d body %q", tc.version, w.Code, w.Body.String())
		}
		if user, err := s.concreteStore(t).GetUser(t.Context(), email); err != nil || user.Role != tc.chatRole {
			t.Fatalf("version %d chat user = (%+v, %v), want role %q", tc.version, user, err, tc.chatRole)
		}
		if _, _, got := opsOf(t, s, email); got != tc.opsRole {
			t.Fatalf("version %d ops role = %q, want %q", tc.version, got, tc.opsRole)
		}
	}
}

// TestExternalAccessProvisioningIsOneAtomicState pins ADR-0074 as amended by
// #2049 M4: an access event writes role, Operations Center scope and enabled
// to the one identity row in one transaction, so there is no "Ops Center
// provisioning failed" 503 and no window where the planes disagree. An event
// older than the stored state changes nothing — the newer state stays whole —
// and a revoke ends Operations Center access with chat access.
func TestExternalAccessProvisioningIsOneAtomicState(t *testing.T) {
	t.Parallel()

	s := memberFixture(t)
	st := s.concreteStore(t)
	const email = "racing@example.com"
	if _, _, err := st.ApplyExternalAccess(t.Context(), store.ExternalAccessState{
		Issuer: "https://auth.example.com", Subject: "racing-account", Email: email,
		Version: 2, Allowed: true, EventID: "grant-2", ChatRole: store.RoleMember,
		OpsRole: "client", IssuedAt: 1_002,
	}); err != nil {
		t.Fatalf("apply the newer desired state: %v", err)
	}
	older := map[string]any{
		"event_id": "grant-1", "issuer": "https://auth.example.com",
		"subject": "racing-account", "action": "grant", "version": 1,
		"issued_at": 1_001,
		"settings":  map[string]string{"chat_role": "viewer", "ops_role": "readonly"},
	}
	if w := do(t, s.Routes(), http.MethodPost, "/auth/external-access", older, email); w.Code != http.StatusNoContent {
		t.Fatalf("older event: status %d body %q", w.Code, w.Body.String())
	}
	if role, scope, legacy := opsOf(t, s, email); role != store.RoleMember || scope != "run" || legacy != "client" {
		t.Fatalf("after an older event the row = %s/%s/%s, want the newer member/run/client", role, scope, legacy)
	}
	// The new scope names are accepted and stored in Auth's legacy names.
	view := map[string]any{
		"event_id": "grant-3", "issuer": "https://auth.example.com",
		"subject": "racing-account", "action": "grant", "version": 3,
		"issued_at": 1_003,
		"settings":  map[string]string{"chat_role": "member", "ops_role": "view"},
	}
	if w := do(t, s.Routes(), http.MethodPost, "/auth/external-access", view, email); w.Code != http.StatusNoContent {
		t.Fatalf("view grant: status %d body %q", w.Code, w.Body.String())
	}
	if state, ok, err := st.ExternalAccessState(t.Context(), "https://auth.example.com", "racing-account"); err != nil || !ok || state.OpsRole != "readonly" {
		t.Fatalf("stored state = %+v %v %v, want ops_role readonly", state, ok, err)
	}
	revoke := map[string]any{
		"event_id": "revoke-4", "issuer": "https://auth.example.com",
		"subject": "racing-account", "action": "revoke", "version": 4,
		"issued_at": 1_004,
		"settings":  map[string]string{"chat_role": "member", "ops_role": "view"},
	}
	if w := do(t, s.Routes(), http.MethodPost, "/auth/external-access", revoke, email); w.Code != http.StatusNoContent {
		t.Fatalf("revoke: status %d body %q", w.Code, w.Body.String())
	}
	users, err := st.ListUsers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Email == email && u.Enabled {
			t.Fatalf("revoked account still enabled: %+v", u)
		}
	}
}
