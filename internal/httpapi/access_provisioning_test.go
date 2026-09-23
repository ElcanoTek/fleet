package httpapi

import (
	"errors"
	"net/http"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

func TestExternalAccessProvisioningDefaultsMemberAndIgnoresStaleVersions(t *testing.T) {
	s := memberFixture(t)
	ops := &fakeOpsAdmins{}
	WithOpsAdmins(ops)(s)
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
	s := memberFixture(t)
	ops := &fakeOpsAdmins{}
	WithOpsAdmins(ops)(s)
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
		if got := ops.roles[email]; got != tc.opsRole {
			t.Fatalf("version %d ops role = %q, want %q", tc.version, got, tc.opsRole)
		}
	}
}

func TestExternalAccessProvisioningReconcilesNewerStateAfterOpsWrite(t *testing.T) {
	s := memberFixture(t)
	st := s.concreteStore(t)
	ops := &fakeOpsAdmins{}
	WithOpsAdmins(ops)(s)
	const email = "racing@example.com"
	ops.onSetRole = func(_ string, role string) {
		if role != "readonly" {
			return
		}
		ops.onSetRole = nil
		_, _, err := st.ApplyExternalAccess(t.Context(), store.ExternalAccessState{
			Issuer: "https://auth.example.com", Subject: "racing-account", Email: email,
			Version: 2, Allowed: true, EventID: "grant-2", ChatRole: store.RoleMember,
			OpsRole: "client", IssuedAt: 1_002,
		})
		if err != nil {
			t.Fatalf("apply concurrent desired state: %v", err)
		}
	}
	body := map[string]any{
		"event_id": "grant-1", "issuer": "https://auth.example.com",
		"subject": "racing-account", "action": "grant", "version": 1,
		"issued_at": 1_001,
		"settings":  map[string]string{"chat_role": "viewer", "ops_role": "readonly"},
	}

	if w := do(t, s.Routes(), http.MethodPost, "/auth/external-access", body, email); w.Code != http.StatusNoContent {
		t.Fatalf("provision: status %d body %q", w.Code, w.Body.String())
	}
	if user, err := st.GetUser(t.Context(), email); err != nil || user.Role != store.RoleMember {
		t.Fatalf("latest chat user = (%+v, %v), want member", user, err)
	}
	if got := ops.roles[email]; got != "client" {
		t.Fatalf("latest Ops role = %q, want client", got)
	}
}
