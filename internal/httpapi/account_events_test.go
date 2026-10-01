package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/accountevents"
	"github.com/ElcanoTek/fleet/internal/store"
)

// opsRoleView reads the fake Ops plane the way the real sched reader does:
// the role of the account's row, "none" without one.
type opsRoleView struct{ ops *fakeOpsAdmins }

func (v opsRoleView) OpsRole(ctx context.Context, email string) (string, error) {
	roles, err := v.ops.Roles(ctx)
	if err != nil {
		return "", err
	}
	if r := roles[strings.ToLower(email)]; r != "" {
		return r, nil
	}
	return "none", nil
}

// failingOps is fakeOpsAdmins whose explicit role writes fail, the way a sched
// DB outage makes setOpsRole log and carry on.
type failingOps struct{ *fakeOpsAdmins }

func (failingOps) SetRole(context.Context, string, string) error {
	return errors.New("sched unavailable")
}

// drainAccountEvents walks the outbox in delivery order.
func drainAccountEvents(t *testing.T, st *store.Store) []store.AccountEvent {
	t.Helper()
	var out []store.AccountEvent
	for {
		batch, err := st.ClaimDueAccountEvents(context.Background(), time.Now().Unix()+1, 100, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) == 0 {
			return out
		}
		for _, ev := range batch {
			out = append(out, ev)
			if err := st.MarkAccountEventDelivered(context.Background(), ev.ID, time.Now().Unix()); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func accountEventsFixture(t *testing.T) (*Server, *fakeOpsAdmins, *store.Store) {
	t.Helper()
	s := memberFixture(t, "boss@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	ops := &fakeOpsAdmins{}
	WithOpsAdmins(ops)(s)
	st := s.concreteStore(t)
	WithAccountEvents(accountevents.NewRecorder(st, opsRoleView{ops}, st))(s)
	return s, ops, st
}

func TestAdminUserChangesPublishAccountEvents(t *testing.T) {
	s, ops, st := accountEventsFixture(t)
	h := s.Routes()

	w := do(t, h, http.MethodPost, "/admin/users",
		map[string]any{"email": "dan@x.com", "password": "dan-pw-12345", "ops_role": "readonly"}, "boss@x.com")
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	// The same values again: not a change.
	w = do(t, h, http.MethodPatch, "/admin/users/dan@x.com",
		map[string]any{"role": "member", "ops_role": "readonly"}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("no-op patch: %d %s", w.Code, w.Body.String())
	}
	// A team-only change is part of the feed's state: it publishes.
	w = do(t, h, http.MethodPatch, "/admin/users/dan@x.com", map[string]any{"team_id": "growth"}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("team patch: %d %s", w.Code, w.Body.String())
	}
	// Re-stating the same team is not a change.
	w = do(t, h, http.MethodPatch, "/admin/users/dan@x.com", map[string]any{"team_id": "growth"}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("same-team patch: %d %s", w.Code, w.Body.String())
	}

	// Chat viewer + Ops client requested, the Ops write fails: the event carries
	// what the planes hold (viewer + readonly), not the request.
	WithOpsAdmins(failingOps{ops})(s)
	w = do(t, h, http.MethodPatch, "/admin/users/dan@x.com",
		map[string]any{"role": "viewer", "ops_role": "client"}, "boss@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	WithOpsAdmins(ops)(s)

	w = do(t, h, http.MethodDelete, "/admin/users/dan@x.com", nil, "boss@x.com")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}

	evs := drainAccountEvents(t, st)
	if len(evs) != 4 {
		t.Fatalf("events = %+v, want create, team, patch, delete", evs)
	}
	want := []struct {
		typ, chat, ops, team string
		enabled              bool
	}{
		{store.AccountEventAccessChanged, "member", "readonly", "", true},
		{store.AccountEventAccessChanged, "member", "readonly", "growth", true},
		{store.AccountEventAccessChanged, "viewer", "readonly", "growth", true},
		{store.AccountEventDeleted, "", "", "", false},
	}
	for i, w := range want {
		ev := evs[i]
		if ev.Type != w.typ || ev.ChatRole != w.chat || ev.OpsRole != w.ops || ev.Team != w.team || ev.Enabled != w.enabled ||
			ev.Email != "dan@x.com" || ev.Source != "admin_ui" || ev.Actor != "boss@x.com" {
			t.Errorf("event %d = %+v, want %+v", i, ev, w)
		}
	}
}

func TestExternalAccessChangesPublishAsIdentityProvider(t *testing.T) {
	s, _, st := accountEventsFixture(t)
	h := s.Routes()
	grant := map[string]any{
		"event_id": "grant-1", "issuer": "https://auth.example.com",
		"subject": "account-9", "action": "grant", "version": 1, "issued_at": 1_000,
		"settings": map[string]string{"chat_role": "member", "ops_role": "client"},
	}
	if w := do(t, h, http.MethodPost, "/auth/external-access", grant, "erin@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("grant: %d %s", w.Code, w.Body.String())
	}
	// The identity provider re-sending the state Fleet already holds (the echo
	// after it adopts a Fleet change) publishes nothing.
	grant["event_id"], grant["version"] = "grant-2", 2
	if w := do(t, h, http.MethodPost, "/auth/external-access", grant, "erin@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("echo: %d %s", w.Code, w.Body.String())
	}
	evs := drainAccountEvents(t, st)
	if len(evs) != 1 {
		t.Fatalf("events = %+v, want exactly the grant", evs)
	}
	if ev := evs[0]; ev.Source != "identity_provider" || ev.Actor != "" || ev.ChatRole != "member" || ev.OpsRole != "client" || !ev.Enabled {
		t.Fatalf("event = %+v", ev)
	}
}

func TestAccountEventsOffQueuesNothing(t *testing.T) {
	s := memberFixture(t, "boss@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	WithOpsAdmins(&fakeOpsAdmins{})(s)
	h := s.Routes()
	w := do(t, h, http.MethodPost, "/admin/users",
		map[string]any{"email": "off@x.com", "password": "off-pw-12345", "role": "admin"}, "boss@x.com")
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	stats, err := s.concreteStore(t).AccountEventStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 0 || stats.Delivered != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want an empty outbox with the feed off", stats)
	}
}

// TestRedeliveredProviderPushDoesNotRevertAFleetChange is the convergence
// case the feed exists for: the provider grants, an admin then changes the
// account's Ops role in Fleet (published admin_ui, which the provider adopts),
// and the provider redelivers its older, already-applied push — ordinary
// at-least-once behaviour. The redelivery must not put the old Ops role back,
// and must publish nothing: a silent revert tagged identity_provider is the
// one event a provider is told it may ignore.
func TestRedeliveredProviderPushDoesNotRevertAFleetChange(t *testing.T) {
	s, ops, st := accountEventsFixture(t)
	h := s.Routes()
	grant := map[string]any{
		"event_id": "grant-1", "issuer": "https://auth.example.com",
		"subject": "account-7", "action": "grant", "version": 1, "issued_at": 1_000,
		"settings": map[string]string{"chat_role": "member", "ops_role": "readonly"},
	}
	if w := do(t, h, http.MethodPost, "/auth/external-access", grant, "gia@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("grant: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, http.MethodPatch, "/admin/users/gia@x.com",
		map[string]any{"ops_role": "client"}, "boss@x.com"); w.Code != http.StatusOK {
		t.Fatalf("admin patch: %d %s", w.Code, w.Body.String())
	}
	// Redelivery of the same, already-applied version.
	if w := do(t, h, http.MethodPost, "/auth/external-access", grant, "gia@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("redelivery: %d %s", w.Code, w.Body.String())
	}
	roles, err := ops.Roles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if roles["gia@x.com"] != "client" {
		t.Fatalf("ops role after redelivery = %q, want the admin's client", roles["gia@x.com"])
	}
	evs := drainAccountEvents(t, st)
	if len(evs) != 2 || evs[0].Source != "identity_provider" || evs[1].Source != "admin_ui" || evs[1].OpsRole != "client" {
		t.Fatalf("events = %+v, want the grant and the admin change only", evs)
	}

	// A deletion in Fleet is adopted too: a redelivered grant does not
	// re-enable the deleted account's Ops identity.
	if w := do(t, h, http.MethodDelete, "/admin/users/gia@x.com", nil, "boss@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	if w := do(t, h, http.MethodPost, "/auth/external-access", grant, "gia@x.com"); w.Code != http.StatusNoContent {
		t.Fatalf("redelivery after delete: %d %s", w.Code, w.Body.String())
	}
	if roles, _ := ops.Roles(context.Background()); roles["gia@x.com"] != "" {
		t.Fatalf("ops role after a redelivered grant to a deleted account = %q, want none", roles["gia@x.com"])
	}
}
