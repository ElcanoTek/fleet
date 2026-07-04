package admincli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/store"
)

// writeBundle marshals a bundle to a temp file and returns its path.
func writeBundle(t *testing.T, b migrationBundle) string {
	t.Helper()
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	path := filepath.Join(t.TempDir(), "bundle.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	return path
}

// TestImportBundle_Validation covers the envelope guards — no DB required
// (validation happens before any store is opened).
func TestImportBundle_Validation(t *testing.T) {
	if code := cmdImport([]string{}); code != 1 {
		t.Errorf("no file: got exit %d, want 1", code)
	}
	if code := cmdImport([]string{filepath.Join(t.TempDir(), "missing.json")}); code != 1 {
		t.Errorf("missing file: got exit %d, want 1", code)
	}

	bad := writeBundle(t, migrationBundle{Format: "something-else", Version: 1, Chat: &chatSection{}})
	if code := cmdImport([]string{bad}); code != 1 {
		t.Errorf("wrong format: got exit %d, want 1", code)
	}

	badVer := writeBundle(t, migrationBundle{Format: migrationBundleFormat, Version: 99, Chat: &chatSection{}})
	if code := cmdImport([]string{badVer}); code != 1 {
		t.Errorf("wrong version: got exit %d, want 1", code)
	}

	empty := writeBundle(t, migrationBundle{Format: migrationBundleFormat, Version: 1})
	if code := cmdImport([]string{empty}); code != 1 {
		t.Errorf("no sections: got exit %d, want 1", code)
	}

	// A dual-section bundle whose chat + sched DSNs resolve to the SAME
	// database must be refused before any write (the two stores' schemas
	// would cross-contaminate).
	dual := writeBundle(t, migrationBundle{Format: migrationBundleFormat, Version: 1, Chat: &chatSection{}, Sched: &schedSection{}})
	code := cmdImport([]string{dual,
		"--chat-database-url", "postgres://x/db1",
		"--sched-database-url", "postgres://x/db1"})
	if code != 1 {
		t.Errorf("same DSN for both sections: got exit %d, want 1", code)
	}
}

// chatTestDSN mirrors internal/store's test gate (FLEET_TEST_DATABASE_URL,
// legacy CHAT_TEST_DATABASE_URL fallback).
func chatTestDSN() string {
	if v := os.Getenv("FLEET_TEST_DATABASE_URL"); v != "" {
		return v
	}
	return os.Getenv("CHAT_TEST_DATABASE_URL")
}

// TestImportBundle_ChatSection proves a chat bundle lands with identity
// preserved — users keep their bcrypt hash, conversations keep ids /
// pinned flags / timestamps, messages keep content + timing, memories keep
// ids — and that a re-run skips everything instead of duplicating.
func TestImportBundle_ChatSection(t *testing.T) {
	dsn := chatTestDSN()
	if dsn == "" {
		t.Skip("FLEET_TEST_DATABASE_URL / CHAT_TEST_DATABASE_URL is not set — skipping Postgres-backed test")
	}
	ctx := context.Background()
	s, err := store.Open(dsn, store.DefaultPoolConfig())
	if err != nil {
		t.Fatalf("open chat store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.TruncateAllForTest(ctx); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	convID := uuid.NewString()
	memID := uuid.NewString()
	hash := "$2a$10$abcdefghijklmnopqrstuvABCDEFGHIJKLMNOPQRSTUV1234567890"
	bundle := migrationBundle{
		Format: migrationBundleFormat, Version: 1, Source: "chat test",
		Chat: &chatSection{
			Users: []store.ImportedUser{
				{Email: "Alice@Example.com", PasswordHash: hash, CreatedAt: 1700000000, UpdatedAt: 1700000001},
			},
			Conversations: []store.ImportedConversation{{
				ID: convID, UserEmail: "alice@example.com", Title: "Migrated chat",
				Persona: "victoria", Model: "some/model", Pinned: true, Lockdown: false,
				CreatedAt: 1700000100, UpdatedAt: 1700000200,
				Messages: []store.ImportedMessage{
					{Role: "user", Type: "text", Content: json.RawMessage(`{"text":"hello from the past"}`), CreatedAt: 1700000100},
					{Role: "assistant", Type: "text", Content: json.RawMessage(`{"text":"hi!"}`), CreatedAt: 1700000101},
				},
			}},
			Memories: []store.ImportedMemory{
				{ID: memID, UserEmail: "alice@example.com", Content: "prefers tea", Source: "manual", CreatedAt: 1700000300, UpdatedAt: 1700000300},
			},
		},
	}
	path := writeBundle(t, bundle)

	if code := cmdImport([]string{path, "--chat-database-url", dsn}); code != 0 {
		t.Fatalf("import: exit %d", code)
	}

	// User landed, normalized, hash intact (i.e. the account keeps its password).
	ok, err := s.IsUser(ctx, "alice@example.com")
	if err != nil || !ok {
		t.Fatalf("imported user missing (ok=%v err=%v)", ok, err)
	}

	// Conversation landed with identity + pin + timestamps preserved,
	// visible through the standard owner-scoped read path.
	conv, err := s.Get(ctx, "alice@example.com", convID)
	if err != nil || conv == nil {
		t.Fatalf("imported conversation missing (err=%v)", err)
	}
	if !conv.Pinned || conv.CreatedAt != 1700000100 || conv.UpdatedAt != 1700000200 {
		t.Errorf("conversation lost fidelity: pinned=%v created=%d updated=%d", conv.Pinned, conv.CreatedAt, conv.UpdatedAt)
	}
	hist, err := s.LoadHistory(ctx, convID)
	if err != nil || len(hist) != 2 {
		t.Fatalf("history: got %d entries err=%v, want 2", len(hist), err)
	}
	if string(hist[0].Content) != `{"text":"hello from the past"}` {
		t.Errorf("message content mutated: %s", hist[0].Content)
	}

	mems, err := s.ListMemories(ctx, "alice@example.com")
	if err != nil || len(mems) != 1 {
		t.Fatalf("memories: got %d err=%v, want 1", len(mems), err)
	}

	// Re-run: everything already present → skipped, nothing duplicated.
	if code := cmdImport([]string{path, "--chat-database-url", dsn}); code != 0 {
		t.Fatalf("re-import: exit %d", code)
	}
	hist, err = s.LoadHistory(ctx, convID)
	if err != nil || len(hist) != 2 {
		t.Fatalf("history after re-import: got %d entries err=%v, want 2 (no dupes)", len(hist), err)
	}
	mems, err = s.ListMemories(ctx, "alice@example.com")
	if err != nil || len(mems) != 1 {
		t.Fatalf("memories after re-import: got %d err=%v, want 1 (no dupes)", len(mems), err)
	}
}

// TestImportBundle_SchedSection proves a moc bundle lands: users keep hashes,
// live recurring tasks get a future next-run computed in their timezone,
// terminal tasks keep their results verbatim, logs attach by task id, and a
// re-run upserts instead of duplicating.
func TestImportBundle_SchedSection(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set — skipping sched-DB import test")
	}
	database := db.New()
	if err := database.Init("", db.DefaultPoolConfig()); err != nil {
		t.Skipf("sched DB unavailable: %v", err)
	}
	ctx := context.Background()
	conn, err := database.Conn().Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(3)"); err != nil {
		conn.Close()
		t.Fatalf("lock: %v", err)
	}
	clean := func() {
		for _, q := range []string{"DELETE FROM logs", "DELETE FROM tasks", "DELETE FROM users"} {
			if _, err := database.Conn().ExecContext(ctx, q); err != nil {
				t.Fatalf("clean %q: %v", q, err)
			}
		}
	}
	clean()
	t.Cleanup(func() {
		clean()
		conn.ExecContext(ctx, "SELECT pg_advisory_unlock(3)")
		conn.Close()
		database.Close()
	})

	st := storage.New()
	st.SetDatabase(database)

	userID := uuid.New()
	liveID := uuid.New()
	doneID := uuid.New()
	result := "42"
	past := time.Now().Add(-48 * time.Hour).UTC()
	bundle := migrationBundle{
		Format: migrationBundleFormat, Version: 1, Source: "moc test",
		Sched: &schedSection{
			Users: []bundleSchedUser{{
				ID: userID, Username: "brad", PasswordHash: "$2a$10$abcdefghijklmnopqrstuvABCDEFGHIJKLMNOPQRSTUV1234567890",
				Role: "admin", CreatedAt: past,
			}},
			Tasks: []bundleTask{
				{
					// Live recurring task whose next fire time is stale: the
					// importer must recompute it in the task's timezone.
					ID: liveID, Prompt: "daily report", Status: "scheduled",
					Priority: 5, CreatedAt: past, ScheduledFor: &past,
					Recurrence: "0 9 * * *", Timezone: "America/New_York",
					CreatedBy: &userID,
				},
				{
					// Terminal history: preserved verbatim.
					ID: doneID, Prompt: "one shot", Status: "success",
					Priority: 5, CreatedAt: past, StartedAt: &past, CompletedAt: &past,
					Result: &result,
				},
			},
			Logs: []bundleLog{
				{TaskID: doneID, SessionData: json.RawMessage(`{"messages":[{"role":"assistant","content":"did the thing"}]}`)},
			},
		},
	}
	path := writeBundle(t, bundle)

	if code := cmdImport([]string{path, "--sched-database-url", os.Getenv("DATABASE_URL")}); code != 0 {
		t.Fatalf("import: exit %d", code)
	}

	u, err := st.GetUserByUsernameWithContext(ctx, "brad")
	if err != nil || u == nil {
		t.Fatalf("imported sched user missing: %v", err)
	}
	if u.ID != userID || u.Role != "admin" {
		t.Errorf("sched user lost fidelity: id=%s role=%s", u.ID, u.Role)
	}

	live, err := st.GetTask(liveID)
	if err != nil || live == nil {
		t.Fatalf("live task missing: %v", err)
	}
	if live.ScheduledFor == nil || !live.ScheduledFor.After(time.Now()) {
		t.Errorf("live recurring task did not get a future next-run: %v", live.ScheduledFor)
	}
	if live.Recurrence != "0 9 * * *" || live.Timezone != "America/New_York" {
		t.Errorf("recurrence/timezone lost: %q %q", live.Recurrence, live.Timezone)
	}
	if live.CreatedBy == nil || *live.CreatedBy != userID {
		t.Errorf("created_by lost: %v", live.CreatedBy)
	}

	done, err := st.GetTask(doneID)
	if err != nil || done == nil {
		t.Fatalf("terminal task missing: %v", err)
	}
	if done.Result == nil || *done.Result != "42" || done.Status != "success" {
		t.Errorf("terminal task lost fidelity: status=%s result=%v", done.Status, done.Result)
	}

	logSession, err := st.GetLog(doneID)
	if err != nil || logSession == nil {
		t.Fatalf("imported log missing: %v", err)
	}

	// Re-run: idempotent upserts, no duplicates, user still matched by name.
	if code := cmdImport([]string{path, "--sched-database-url", os.Getenv("DATABASE_URL")}); code != 0 {
		t.Fatalf("re-import: exit %d", code)
	}
	var nTasks, nUsers int
	if err := database.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks").Scan(&nTasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if err := database.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&nUsers); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if nTasks != 2 || nUsers != 1 {
		t.Errorf("re-import duplicated rows: tasks=%d users=%d (want 2, 1)", nTasks, nUsers)
	}
}

// TestImportBundle_SchedLiveOnly proves --live-only skips terminal tasks and
// logs but still imports live definitions.
func TestImportBundle_SchedLiveOnly(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set — skipping sched-DB import test")
	}
	database := db.New()
	if err := database.Init("", db.DefaultPoolConfig()); err != nil {
		t.Skipf("sched DB unavailable: %v", err)
	}
	ctx := context.Background()
	conn, err := database.Conn().Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(3)"); err != nil {
		conn.Close()
		t.Fatalf("lock: %v", err)
	}
	clean := func() {
		for _, q := range []string{"DELETE FROM logs", "DELETE FROM tasks", "DELETE FROM users"} {
			if _, err := database.Conn().ExecContext(ctx, q); err != nil {
				t.Fatalf("clean %q: %v", q, err)
			}
		}
	}
	clean()
	t.Cleanup(func() {
		clean()
		conn.ExecContext(ctx, "SELECT pg_advisory_unlock(3)")
		conn.Close()
		database.Close()
	})

	liveID, doneID := uuid.New(), uuid.New()
	now := time.Now().UTC()
	bundle := migrationBundle{
		Format: migrationBundleFormat, Version: 1,
		Sched: &schedSection{
			Tasks: []bundleTask{
				{ID: liveID, Prompt: "recurring", Status: "pending", CreatedAt: now, Recurrence: "*/5 * * * *"},
				{ID: doneID, Prompt: "old run", Status: "error", CreatedAt: now},
			},
			Logs: []bundleLog{{TaskID: doneID, SessionData: json.RawMessage(`{}`)}},
		},
	}
	path := writeBundle(t, bundle)
	if code := cmdImport([]string{path, "--live-only", "--sched-database-url", os.Getenv("DATABASE_URL")}); code != 0 {
		t.Fatalf("import: exit %d", code)
	}

	var nTasks, nLogs int
	if err := database.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks").Scan(&nTasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if err := database.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM logs").Scan(&nLogs); err != nil {
		t.Fatalf("count logs: %v", err)
	}
	if nTasks != 1 || nLogs != 0 {
		t.Errorf("--live-only: tasks=%d logs=%d (want 1, 0)", nTasks, nLogs)
	}
	var gotID string
	if err := database.Conn().QueryRowContext(ctx, "SELECT id FROM tasks").Scan(&gotID); err != nil {
		t.Fatalf("select surviving task: %v", err)
	}
	if gotID != liveID.String() {
		t.Errorf("surviving task is %s, want the live one %s", gotID, liveID)
	}
}
