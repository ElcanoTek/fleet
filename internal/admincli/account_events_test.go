package admincli

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	// `chat user del` removes the Chat account only; the Ops identity stays
	// (ADR-0005), so the feed reports that remaining access rather than a
	// deletion. `sched user del` then removes it, and resync reports the
	// account gone.
	if code := cmdSched([]string{"user", "del", email}); code != 0 {
		t.Fatalf("sched user del: exit %d", code)
	}
	captureStdout(t, func() {
		if code := cmdAccountEvents([]string{"resync"}); code != 0 {
			t.Errorf("resync: exit %d", code)
		}
	})

	evs := drainCLIEvents(t, chat)
	if len(evs) != 4 {
		t.Fatalf("events = %+v, want chat role, ops role, residual ops, delete", evs)
	}
	checks := []struct {
		typ, chatRole, opsRole, source string
		enabled                        bool
	}{
		{store.AccountEventAccessChanged, "viewer", "readonly", "cli", true},
		{store.AccountEventAccessChanged, "viewer", "client", "cli", true},
		{store.AccountEventAccessChanged, "", "client", "cli", false},
		{store.AccountEventDeleted, "", "", "resync", false},
	}
	for i, c := range checks {
		if ev := evs[i]; ev.Type != c.typ || ev.ChatRole != c.chatRole || ev.OpsRole != c.opsRole ||
			ev.Source != c.source || ev.Enabled != c.enabled || ev.Email != email {
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

// TestCLIResyncRepublishesALostDeletion: an account deleted with no event
// queued (the crash window, a CLI that could not open a database) is known
// to the feed from its outbox history, and resync reports it gone.
func TestCLIResyncRepublishesALostDeletion(t *testing.T) {
	chat, _ := accountEventsCLIFixture(t)
	ctx := context.Background()
	if _, err := chat.CreateUser(ctx, "lost@acctev.test", "lost-password-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := chat.EnqueueAccountEvent(ctx, store.AccountEvent{
		Type: store.AccountEventAccessChanged, Email: "lost@acctev.test", Enabled: true,
		ChatRole: store.RoleMember, OpsRole: "none", Source: store.AccountEventSourceCLI,
	}); err != nil {
		t.Fatal(err)
	}
	drainCLIEvents(t, chat)
	if err := chat.DeleteUser(ctx, "lost@acctev.test"); err != nil { // no event: the lost one
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if code := cmdAccountEvents([]string{"resync"}); code != 0 {
			t.Errorf("resync: exit %d", code)
		}
	})
	evs := drainCLIEvents(t, chat)
	if len(evs) != 1 || evs[0].Type != store.AccountEventDeleted || evs[0].Email != "lost@acctev.test" || evs[0].Source != "resync" {
		t.Fatalf("resync events = %+v, want the lost deletion", evs)
	}
	if !strings.Contains(out, "(1 deletion(s))") {
		t.Fatalf("resync output = %q", out)
	}
	// Once reported, a second resync does not repeat it.
	captureStdout(t, func() { cmdAccountEvents([]string{"resync"}) })
	if evs := drainCLIEvents(t, chat); len(evs) != 0 {
		t.Fatalf("second resync = %+v, want nothing", evs)
	}
}

// TestCLIWarnsWhenTheServerEnvFileIsUnreadable: a CLI that cannot read the
// env file cannot know whether the feed is on, and says so instead of
// silently publishing nothing.
func TestCLIWarnsWhenTheServerEnvFileIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file; the unreadable case cannot be staged")
	}
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(envFile, []byte("FLEET_ACCOUNT_EVENTS_URL=https://auth.example.com/e\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_ENV_FILE", envFile)
	t.Setenv("FLEET_ACCOUNT_EVENTS_URL", "")
	resetEnvFileCache()
	warnEnvFileUnreadable = sync.Once{}
	t.Cleanup(resetEnvFileCache)

	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	on := accountEventsConfigured()
	os.Stderr = orig
	_ = w.Close()
	msg, _ := io.ReadAll(r)
	if on {
		t.Fatal("feed reported on from a file that could not be read")
	}
	if !strings.Contains(string(msg), "cannot tell whether the account-events feed is on") {
		t.Fatalf("stderr = %q, want the unreadable-env-file warning", msg)
	}
	// resync refuses rather than queue rows a feed that may be off never drains.
	if code := accountEventsResync(nil); code == 0 {
		t.Fatal("resync ran with the feed state unknown")
	}
	if accountEventsFeed() != feedUnknown {
		t.Fatal("feed state should be unknown")
	}
}

// TestImportPublishesCreatedAccounts: `fleet import` writes Chat users and Ops
// identities in separate sections, and afterwards publishes each created Chat
// account once with its state across both; an Ops-only name publishes nothing.
func TestImportPublishesCreatedAccounts(t *testing.T) {
	chat, sched := accountEventsCLIFixture(t)
	ctx := context.Background()
	if _, err := chat.CreateUser(ctx, "imp@acctev.test", "import-password-1"); err != nil {
		t.Fatal(err)
	}
	if err := sched.EnsureUserWithRole(ctx, "imp@acctev.test", "client"); err != nil {
		t.Fatal(err)
	}
	if err := sched.EnsureUserWithRole(ctx, "api@acctev.test", "readonly"); err != nil {
		t.Fatal(err)
	}
	publishImportedAccounts(ctx, "", "", []string{"imp@acctev.test", "api@acctev.test", "IMP@acctev.test"})
	evs := drainCLIEvents(t, chat)
	if len(evs) != 1 || evs[0].Email != "imp@acctev.test" || evs[0].OpsRole != "client" || evs[0].Source != "cli" {
		t.Fatalf("events = %+v, want one cli event for the imported Chat account", evs)
	}
}

// TestEnvShowMasksReceiverURLPaths: a receiver URL often carries its own
// credential in the path or query under a name no secret heuristic matches;
// `fleet env show` prints scheme and host only.
func TestEnvShowMasksReceiverURLPaths(t *testing.T) {
	for key, in := range map[string]string{
		"FLEET_ACCOUNT_EVENTS_URL": "https://auth.example.com/apps/fleet/events?token=sekrit",
		"FLEET_WEBHOOK_URL":        "https://hooks.example.com/services/T0/B0/sekrit",
	} {
		got := redactEnvValue(key, in)
		if strings.Contains(got, "sekrit") || !strings.HasPrefix(got, "https://") || !strings.Contains(got, ".example.com") {
			t.Errorf("%s shown as %q", key, got)
		}
	}
	if got := redactEnvValue("FLEET_ACCOUNT_EVENTS_URL", "https://auth.example.com"); got != "https://auth.example.com" {
		t.Errorf("a bare origin shown as %q", got)
	}
}
