package httpapi

import (
	"context"
	"net/http"
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
	s, _, st := accountEventsFixture(t)
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
	evs := drainAccountEvents(t, st)
	if len(evs) != 2 || evs[0].Team != "growth" || evs[0].Source != "identity_provider" || evs[1].ChatRole != "viewer" {
		t.Fatalf("events = %+v, want the grant (team growth) and the role change, both identity_provider", evs)
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

// A team-only change an admin makes to a provider-managed account is adopted
// into the provider's stored state, so a redelivered push cannot revert it.
func TestRedeliveredProviderPushDoesNotRevertAFleetTeamChange(t *testing.T) {
	s, _, st := accountEventsFixture(t)
	h := s.Routes()
	grant := externalGrant("acct-r", 1, map[string]string{"chat_role": "member", "ops_role": "none", "team": "growth"})
	if w := do(t, h, http.MethodPost, "/auth/external-access", grant, "ray@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("grant: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, http.MethodPatch, "/admin/users/ray@x.com", map[string]any{"team_id": "quant"}, "boss@x.com"); w.Code != http.StatusOK {
		t.Fatalf("admin team patch: %d %s", w.Code, w.Body.String())
	}
	state, ok, err := st.ExternalAccessState(context.Background(), "https://auth.example.com", "acct-r")
	if err != nil || !ok || state.Team == nil || *state.Team != "quant" {
		t.Fatalf("provider state = (%+v, %v, %v), want team quant adopted", state, ok, err)
	}
	if w := do(t, h, http.MethodPost, "/auth/external-access", grant, "ray@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("redelivery: %d %s", w.Code, w.Body.String())
	}
	if got := accountTeam(t, st, "ray@x.com"); got != "quant" {
		t.Fatalf("team after redelivery = %q, want the admin's quant", got)
	}
	evs := drainAccountEvents(t, st)
	if len(evs) != 2 || evs[1].Source != "admin_ui" || evs[1].Team != "quant" {
		t.Fatalf("events = %+v, want the grant and the admin team change only", evs)
	}
}

// A team rename publishes one event per member, with the admin as actor.
func TestTeamRenamePublishesEachMember(t *testing.T) {
	s, _, st := accountEventsFixture(t)
	h := s.Routes()
	for _, email := range []string{"ann@x.com", "ben@x.com"} {
		if w := do(t, h, http.MethodPost, "/admin/users",
			map[string]any{"email": email, "password": "long-enough-pw", "team_id": "devops"}, "boss@x.com"); w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", email, w.Code, w.Body.String())
		}
	}
	_ = drainAccountEvents(t, st)
	if w := do(t, h, http.MethodPost, "/admin/teams/rename", map[string]any{"from": "devops", "to": "platform"}, "boss@x.com"); w.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	evs := drainAccountEvents(t, st)
	if len(evs) != 2 {
		t.Fatalf("events = %+v, want one per member", evs)
	}
	for i, email := range []string{"ann@x.com", "ben@x.com"} {
		if ev := evs[i]; ev.Email != email || ev.Team != "platform" || ev.Source != "admin_ui" || ev.Actor != "boss@x.com" {
			t.Errorf("event %d = %+v, want %s moved to platform by boss", i, ev, email)
		}
	}
	// A rename that fails (no such team) publishes nothing.
	if w := do(t, h, http.MethodPost, "/admin/teams/rename", map[string]any{"from": "nope", "to": "x"}, "boss@x.com"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad rename: %d %s", w.Code, w.Body.String())
	}
	if evs := drainAccountEvents(t, st); len(evs) != 0 {
		t.Fatalf("a failed rename published %+v", evs)
	}
}

// A person moving themselves (self-serve /me/team) publishes like an admin
// move, with themselves as actor.
func TestSelfServeTeamChangePublishes(t *testing.T) {
	s, _, st := accountEventsFixture(t)
	h := s.Routes()
	if w := do(t, h, http.MethodPost, "/admin/users",
		map[string]any{"email": "sam@x.com", "password": "long-enough-pw"}, "boss@x.com"); w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	_ = drainAccountEvents(t, st)
	if w := do(t, h, http.MethodPut, "/me/team", map[string]any{"team_id": "fresh-team"}, "sam@x.com"); w.Code != http.StatusOK {
		t.Fatalf("self-serve: %d %s", w.Code, w.Body.String())
	}
	evs := drainAccountEvents(t, st)
	if len(evs) != 1 || evs[0].Team != "fresh-team" || evs[0].Actor != "sam@x.com" || evs[0].Source != "admin_ui" {
		t.Fatalf("events = %+v, want sam's own move", evs)
	}
}
