package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

func externalGrant(subject string, version int, settings map[string]string) map[string]any {
	return map[string]any{
		"event_id": "grant", "issuer": "https://auth.example.com",
		"subject": subject, "action": "grant", "version": version, "issued_at": 1_000,
		"settings": settings,
	}
}

// teamSyncFixture is a chat-server with an admin (boss) and a fake Ops plane,
// over the real chat store.
func teamSyncFixture(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	s := memberFixture(t, "boss@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	WithOpsAdmins(&fakeOpsAdmins{})(s)
	return s, s.concreteStore(t)
}

func accountTeam(t *testing.T, st *store.Store, email string) string {
	t.Helper()
	a, ok, err := st.AccountAccess(context.Background(), email)
	if err != nil || !ok {
		t.Fatalf("AccountAccess %s = (%v, %v)", email, ok, err)
	}
	return a.Team
}

// The provider's settings may carry a third key, team: applied when present,
// the team left alone when absent, and any other extra key still refused.
func TestExternalAccessAppliesProviderTeam(t *testing.T) {
	s, st := teamSyncFixture(t)
	h := s.Routes()
	withTeam := externalGrant("acct-t", 1, map[string]string{"chat_role": "member", "ops_role": "none", "team": "growth"})
	if w := do(t, h, http.MethodPost, "/auth/external-access", withTeam, "tia@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("grant with team: %d %s", w.Code, w.Body.String())
	}
	if got := accountTeam(t, st, "tia@x.com"); got != "growth" {
		t.Fatalf("team = %q, want growth", got)
	}
	noTeam := externalGrant("acct-t", 2, map[string]string{"chat_role": "viewer", "ops_role": "none"})
	if w := do(t, h, http.MethodPost, "/auth/external-access", noTeam, "tia@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("grant without team: %d %s", w.Code, w.Body.String())
	}
	if got := accountTeam(t, st, "tia@x.com"); got != "growth" {
		t.Fatalf("team after a grant without team = %q, want growth untouched", got)
	}

	for name, settings := range map[string]map[string]string{
		"unknown key":   {"chat_role": "member", "ops_role": "none", "colour": "blue"},
		"team + extra":  {"chat_role": "member", "ops_role": "none", "team": "a", "colour": "blue"},
		"control char":  {"chat_role": "member", "ops_role": "none", "team": "bad\x07"},
		"too long team": {"chat_role": "member", "ops_role": "none", "team": string(make([]byte, 65))},
	} {
		if w := do(t, h, http.MethodPost, "/auth/external-access", externalGrant("acct-t", 3, settings), "tia@x.com"); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, w.Code, w.Body.String())
		}
	}
}

// Every admin write enforces the shared team rule, so Fleet never stores a
// label its identity provider would refuse to mirror.
func TestAdminTeamWritesEnforceTheTeamRule(t *testing.T) {
	s, st := teamSyncFixture(t)
	h := s.Routes()
	astral := strings.Repeat("\U0001F600", 17) // 68 bytes, 34 UTF-16 units: under a browser maxlength of 64
	if w := do(t, h, http.MethodPost, "/admin/users", map[string]any{"email": "neo@x.com", "password": "long-enough-pw", "team_id": astral}, "boss@x.com"); w.Code != http.StatusBadRequest {
		t.Fatalf("create with an over-long team: %d %s, want 400", w.Code, w.Body.String())
	}
	if _, err := st.GetUser(context.Background(), "neo@x.com"); err == nil {
		t.Fatal("a create refused for its team still created the account")
	}
	if w := do(t, h, http.MethodPost, "/admin/users", map[string]any{"email": "neo@x.com", "password": "long-enough-pw"}, "boss@x.com"); w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	for name, team := range map[string]string{"over-long": astral, "control char": "ops\x07"} {
		if w := do(t, h, http.MethodPatch, "/admin/users/neo@x.com", map[string]any{"team_id": team}, "boss@x.com"); w.Code != http.StatusBadRequest {
			t.Errorf("patch %s: %d %s, want 400", name, w.Code, w.Body.String())
		}
	}
	if w := do(t, h, http.MethodPatch, "/admin/users/neo@x.com", map[string]any{"team_id": "  quant  "}, "boss@x.com"); w.Code != http.StatusOK {
		t.Fatalf("valid patch: %d %s", w.Code, w.Body.String())
	}
	if got := accountTeam(t, st, "neo@x.com"); got != "quant" {
		t.Fatalf("team = %q, want quant", got)
	}
	for name, team := range map[string]string{"over-long": astral, "control char": "ops\x07"} {
		if w := do(t, h, http.MethodPut, "/me/team", map[string]any{"team_id": team}, "neo@x.com"); w.Code != http.StatusBadRequest {
			t.Errorf("self-serve %s: %d %s, want 400", name, w.Code, w.Body.String())
		}
	}
	if w := do(t, h, http.MethodPost, "/admin/teams/rename", map[string]any{"from": "quant", "to": astral}, "boss@x.com"); w.Code != http.StatusBadRequest {
		t.Errorf("rename onto an over-long name: %d %s, want 400", w.Code, w.Body.String())
	}
	if _, _, err := st.RenameTeam(context.Background(), "quant", astral); !store.IsInputError(err) {
		t.Fatalf("rename onto an over-long name: err = %v, want an input error", err)
	}
}
