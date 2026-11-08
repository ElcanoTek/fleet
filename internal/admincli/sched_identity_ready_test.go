package admincli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/testdb"
)

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	os.Stderr = orig
	_ = w.Close()
	return <-done
}

// A sched-only verb run first on a fresh one-database fleet works (#2049 M6):
// the scheduler's open runs the one migration runner, which migrates chat,
// core and sched together, so the identity table exists before any sched
// verb reads it. (Before M6 such a verb could run ahead of the chat store and
// was refused with what to do; Codex 4504695148 on #2083.)
func TestSchedUserVerbsWorkSchedFirst(t *testing.T) {
	dsn := testdb.Empty(t)
	schedURL := config.WithSchema(dsn, config.SchedSchema)

	var code int
	out := captureStderr(t, func() { code = cmdSchedUser([]string{"list", "--database-url", schedURL}) })
	if code != 0 || strings.Contains(out, "does not exist yet") {
		t.Fatalf("sched user list on a fresh database: exit %d, stderr %q", code, out)
	}
	if code := cmdSchedUser([]string{"add", "ops@x.com", "--password", "placeholder-not-a-secret", "--role", "client", "--database-url", schedURL}); code != 0 {
		t.Fatalf("sched user add on a fresh database: exit %d", code)
	}
	if code := cmdSchedUser([]string{"set-role", "ops@x.com", "--role", "readonly", "--database-url", schedURL}); code != 0 {
		t.Fatalf("sched user set-role on a fresh database: exit %d", code)
	}
	if code := cmdSchedBudget([]string{"list", "--database-url", schedURL}); code != 0 {
		t.Fatalf("sched budget list sched-first: exit %d", code)
	}
	// The chat store sees the account the scheduler verb created: one table.
	chat, err := store.Open(config.WithSchema(dsn, config.ChatSchema), store.DefaultPoolConfig())
	if err != nil {
		t.Fatalf("open chat store: %v", err)
	}
	defer chat.Close()
	users, err := chat.ListUsers(t.Context())
	if err != nil || len(users) != 1 || users[0].Email != "ops@x.com" {
		t.Fatalf("chat ListUsers = %+v, %v; want ops@x.com", users, err)
	}
}
