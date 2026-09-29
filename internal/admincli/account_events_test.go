package admincli

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/accountevents"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/store"
)

// accountEventsCLIFixture points the CLI at the test databases through the
// deployment env (the way a provisioned box resolves them) with the feed on.
func accountEventsCLIFixture(t *testing.T) (*store.Store, *storage.Storage) {
	t.Helper()
	chatDsn, schedDsn := chatTestDSN(), os.Getenv("DATABASE_URL")
	if chatDsn == "" || schedDsn == "" {
		t.Skip("FLEET_TEST_DATABASE_URL and DATABASE_URL are required; skipping Postgres-backed test")
	}
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(envFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_ENV_FILE", envFile)
	t.Setenv("FLEET_CHAT_DATABASE_URL", chatDsn)
	t.Setenv("FLEET_SCHED_DATABASE_URL", schedDsn)
	t.Setenv("FLEET_ACCOUNT_EVENTS_URL", "http://127.0.0.1:9/events")

	ctx := context.Background()
	chat, err := store.Open(chatDsn, store.DefaultPoolConfig())
	if err != nil {
		t.Fatalf("open chat store: %v", err)
	}
	t.Cleanup(func() { _ = chat.Close() })
	if err := chat.TruncateAllForTest(ctx); err != nil {
		t.Fatalf("truncate chat: %v", err)
	}
	sched := storage.New()
	if err := sched.Initialize(schedDsn, storage.DefaultPoolConfig()); err != nil {
		t.Fatalf("open sched: %v", err)
	}
	t.Cleanup(func() { _ = sched.Close() })
	if _, err := sched.DB().Conn().ExecContext(ctx, `DELETE FROM users WHERE username LIKE '%@acctev.test'`); err != nil {
		t.Fatalf("clean sched users: %v", err)
	}
	return chat, sched
}

func drainCLIEvents(t *testing.T, st *store.Store) []store.AccountEvent {
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

func TestCLIRoleCommandsQueueAccountEvents(t *testing.T) {
	chat, sched := accountEventsCLIFixture(t)
	ctx := context.Background()
	const email = "cli@acctev.test"
	if _, err := chat.CreateUser(ctx, email, "cli-password-1"); err != nil {
		t.Fatal(err)
	}
	if err := sched.EnsureUserWithRole(ctx, email, "readonly"); err != nil {
		t.Fatal(err)
	}

	if code := cmdChat([]string{"user", "role", email, "--role", "viewer"}); code != 0 {
		t.Fatalf("chat user role: exit %d", code)
	}
	if code := cmdSched([]string{"user", "set-role", email, "--role", "client"}); code != 0 {
		t.Fatalf("sched user set-role: exit %d", code)
	}
	// Re-asserting the role is not a change.
	if code := cmdSched([]string{"user", "set-role", email, "--role", "client"}); code != 0 {
		t.Fatalf("sched user set-role (no-op): exit %d", code)
	}
	if code := cmdChat([]string{"user", "del", email}); code != 0 {
		t.Fatalf("chat user del: exit %d", code)
	}

	evs := drainCLIEvents(t, chat)
	if len(evs) != 3 {
		t.Fatalf("events = %+v, want chat role, ops role, delete", evs)
	}
	checks := []struct{ typ, chatRole, opsRole string }{
		{store.AccountEventAccessChanged, "viewer", "readonly"},
		{store.AccountEventAccessChanged, "viewer", "client"},
		{store.AccountEventDeleted, "", ""},
	}
	for i, c := range checks {
		if ev := evs[i]; ev.Type != c.typ || ev.ChatRole != c.chatRole || ev.OpsRole != c.opsRole || ev.Source != "cli" || ev.Email != email {
			t.Errorf("event %d = %+v, want %+v", i, ev, c)
		}
	}
}

func TestCLIFeedOffQueuesNothing(t *testing.T) {
	chat, _ := accountEventsCLIFixture(t)
	t.Setenv("FLEET_ACCOUNT_EVENTS_URL", "")
	ctx := context.Background()
	if _, err := chat.CreateUser(ctx, "off@acctev.test", "off-password-1"); err != nil {
		t.Fatal(err)
	}
	if code := cmdChat([]string{"user", "role", "off@acctev.test", "--role", "admin"}); code != 0 {
		t.Fatalf("chat user role: exit %d", code)
	}
	if evs := drainCLIEvents(t, chat); len(evs) != 0 {
		t.Fatalf("feed off queued %+v", evs)
	}
	if code := cmdAccountEvents([]string{"resync"}); code != 1 {
		t.Fatalf("resync with the feed off: exit %d, want 1", code)
	}
}

func TestCLIExportAndResync(t *testing.T) {
	chat, sched := accountEventsCLIFixture(t)
	ctx := context.Background()
	for _, e := range []string{"a@acctev.test", "b@acctev.test"} {
		if _, err := chat.CreateUser(ctx, e, "export-password-1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := sched.EnsureAdminUser(ctx, "a@acctev.test"); err != nil {
		t.Fatal(err)
	}
	admin := "admin"
	if _, err := chat.SetUserRoleTeam(ctx, "a@acctev.test", &admin, nil); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if code := cmdAccountEvents([]string{"export"}); code != 0 {
			t.Errorf("export: exit %d", code)
		}
	})
	var users []accountevents.PayloadUser
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var u accountevents.PayloadUser
		if err := json.Unmarshal(sc.Bytes(), &u); err != nil {
			t.Fatalf("export line %q: %v", sc.Text(), err)
		}
		users = append(users, u)
	}
	want := []accountevents.PayloadUser{
		{Email: "a@acctev.test", Enabled: true, ChatRole: "admin", OpsRole: "admin"},
		{Email: "b@acctev.test", Enabled: true, ChatRole: "member", OpsRole: "none"},
	}
	if len(users) != len(want) || users[0] != want[0] || users[1] != want[1] {
		t.Fatalf("export = %+v, want %+v", users, want)
	}
	if evs := drainCLIEvents(t, chat); len(evs) != 0 {
		t.Fatalf("export queued %+v; it must be read-only", evs)
	}

	captureStdout(t, func() {
		if code := cmdAccountEvents([]string{"resync"}); code != 0 {
			t.Errorf("resync: exit %d", code)
		}
	})
	evs := drainCLIEvents(t, chat)
	if len(evs) != 2 || evs[0].Source != "resync" || evs[0].ChatRole != "admin" || evs[1].Email != "b@acctev.test" {
		t.Fatalf("resync events = %+v", evs)
	}

	status := captureStdout(t, func() {
		if code := cmdAccountEvents([]string{"status"}); code != 0 {
			t.Errorf("status: exit %d", code)
		}
	})
	if !strings.Contains(status, "feed:      on") || !strings.Contains(status, "delivered: 2") || strings.Contains(status, "127.0.0.1") {
		t.Fatalf("status output:\n%s", status)
	}
}
