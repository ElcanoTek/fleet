package admincli

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/store"
)

// exportCLIFixture points the CLI at the test databases through the
// deployment env, the way a provisioned box resolves them.
func exportCLIFixture(t *testing.T) (*store.Store, *storage.Storage) {
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

// TestCLIExport: every Chat account, disabled ones included, with its Chat
// role, team and effective Ops role, in the field names central Auth's team
// import reads.
func TestCLIExport(t *testing.T) {
	chat, sched := exportCLIFixture(t)
	ctx := context.Background()
	for _, e := range []string{"a@acctev.test", "b@acctev.test", "c@acctev.test"} {
		if _, err := chat.CreateUser(ctx, e, "export-password-1"); err != nil {
			t.Fatal(err)
		}
	}
	if err := sched.EnsureAdminUser(ctx, "a@acctev.test"); err != nil {
		t.Fatal(err)
	}
	admin, team := "admin", "Reklaim Internal"
	if _, err := chat.SetUserRoleTeam(ctx, "a@acctev.test", &admin, &team); err != nil {
		t.Fatal(err)
	}
	// Central Auth's revoke leaves the account disabled, not deleted.
	db, err := sql.Open("pgx", chatTestDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE users SET enabled = FALSE WHERE email = 'c@acctev.test'`); err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		if code := cmdAccountEvents([]string{"export"}); code != 0 {
			t.Errorf("export: exit %d", code)
		}
	})
	var lines []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var line map[string]any
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("export line %q: %v", sc.Text(), err)
		}
		lines = append(lines, line)
	}
	want := []map[string]any{
		{"email": "a@acctev.test", "enabled": true, "chat_role": "admin", "ops_role": "admin", "team": "Reklaim Internal"},
		{"email": "b@acctev.test", "enabled": true, "chat_role": "member", "ops_role": "none", "team": ""},
		{"email": "c@acctev.test", "enabled": false, "chat_role": "member", "ops_role": "none", "team": ""},
	}
	if len(lines) != len(want) {
		t.Fatalf("export = %v, want %v", lines, want)
	}
	for i := range want {
		if len(lines[i]) != len(want[i]) {
			t.Errorf("line %d = %v, want %v", i, lines[i], want[i])
		}
		for k, v := range want[i] {
			if lines[i][k] != v {
				t.Errorf("line %d %s = %v, want %v", i, k, lines[i][k], v)
			}
		}
	}

	if code := cmdAccountEvents([]string{"resync"}); code == 0 {
		t.Error("resync is gone with the feed; it must not succeed")
	}
}

// TestEnvShowMasksReceiverURLPaths: an outbound receiver URL shows scheme and
// host only. FLEET_ACCOUNT_EVENTS_URL is retired (ADR-0096) but may linger in
// an env file until the boot warning gets it removed.
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
