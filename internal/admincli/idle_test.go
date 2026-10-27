package admincli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

func TestIdleLibSplitDSNPassword(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := repoRootFromTest(t)
	lib := filepath.Join(root, "scripts", "lib", "idle.sh")

	cases := []struct {
		dsn      string
		wantPass string
		wantRed  string
	}{
		{
			dsn:      "postgres://fleet_sched:s3cr3t!pw@127.0.0.1:5432/sched?sslmode=disable",
			wantPass: "s3cr3t!pw",
			wantRed:  "postgres://fleet_sched@127.0.0.1:5432/sched?sslmode=disable",
		},
		{
			dsn:      "postgres://fleet_sched:s3cr3t%40%3A%25!pw@127.0.0.1:5432/sched?sslmode=disable",
			wantPass: "s3cr3t@:%!pw",
			wantRed:  "postgres://fleet_sched@127.0.0.1:5432/sched?sslmode=disable",
		},
		{
			dsn:      "postgres://fleet_chat@127.0.0.1:5432/chat?sslmode=disable",
			wantPass: "",
			wantRed:  "postgres://fleet_chat@127.0.0.1:5432/chat?sslmode=disable",
		},
		{
			dsn:      "postgres://127.0.0.1:5432/chat",
			wantPass: "",
			wantRed:  "postgres://127.0.0.1:5432/chat",
		},
		{
			dsn:      "postgresql://127.0.0.1:5432/sched?user=fleet&password=s3cr3t%21pw&sslmode=disable",
			wantPass: "s3cr3t!pw",
			wantRed:  "postgresql://127.0.0.1:5432/sched?user=fleet&sslmode=disable",
		},
		{
			dsn:      "postgresql://127.0.0.1:5432/sched?password=secret",
			wantPass: "secret",
			wantRed:  "postgresql://127.0.0.1:5432/sched",
		},
		{
			dsn:      "host=127.0.0.1 user=fleet password=s3cr3t!pw dbname=sched",
			wantPass: "s3cr3t!pw",
			wantRed:  "host=127.0.0.1 user=fleet dbname=sched",
		},
		{
			dsn:      "host=127.0.0.1 user=fleet password='s3cr3t pw' dbname=sched",
			wantPass: "s3cr3t pw",
			wantRed:  "host=127.0.0.1 user=fleet dbname=sched",
		},
		{
			dsn:      `host=127.0.0.1 user=fleet password=foo\ bar dbname=sched`,
			wantPass: "foo bar",
			wantRed:  "host=127.0.0.1 user=fleet dbname=sched",
		},
	}

	for _, tc := range cases {
		cmd := exec.Command("bash", "-c", `. "$1"; fleet_split_dsn_password "$2"`, "_", lib, tc.dsn)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fleet_split_dsn_password(%q): %v\n%s", tc.dsn, err, string(out))
		}
		lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("expected 2 lines, got %q", string(out))
		}
		if lines[0] != tc.wantPass || lines[1] != tc.wantRed {
			t.Errorf("dsn=%q: got pass=%q redacted=%q, want pass=%q redacted=%q",
				tc.dsn, lines[0], lines[1], tc.wantPass, tc.wantRed)
		}
	}

	// Verify fleet_resolve_dsn prefers FLEET_SCHED_DATABASE_URL in the env file
	// over a generic DATABASE_URL in the calling shell's environment.
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(envFile, []byte("FLEET_SCHED_DATABASE_URL=postgres://specific@127.0.0.1:5432/sched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", `. "$1"; fleet_resolve_dsn FLEET_SCHED_DATABASE_URL "$2"`, "_", lib, envFile)
	cmd.Env = append(os.Environ(), "DATABASE_URL=postgres://generic@127.0.0.1:5432/generic", "FLEET_SCHED_DATABASE_URL=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fleet_resolve_dsn: %v\n%s", err, string(out))
	}
	if got := strings.TrimSpace(string(out)); got != "postgres://specific@127.0.0.1:5432/sched" {
		t.Errorf("fleet_resolve_dsn got %q, want specific DSN from env file", got)
	}

	// Verify DB_HOST/DB_USER/DB_PASSWORD fallback in fleet_resolve_dsn (including UTF-8 bytes).
	dbPartsEnv := filepath.Join(t.TempDir(), "parts.env")
	if err := os.WriteFile(dbPartsEnv, []byte("DB_HOST=db.internal\nDB_PASSWORD=päss:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("bash", "-c", `. "$1"; fleet_resolve_dsn FLEET_SCHED_DATABASE_URL "$2"`, "_", lib, dbPartsEnv)
	cmd.Env = append(os.Environ(), "DATABASE_URL=", "FLEET_SCHED_DATABASE_URL=", "SCHED_DATABASE_URL=", "DB_HOST=", "DB_PASSWORD=")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fleet_resolve_dsn DB_* parts: %v\n%s", err, string(out))
	}
	if got := strings.TrimSpace(string(out)); got != "postgres://fleet:p%C3%A4ss%3A1@db.internal:5432/sched?sslmode=disable" {
		t.Errorf("fleet_resolve_dsn DB_* parts got %q", got)
	}

	// Verify unconditional localhost:5432 defaults when no URL or DB_* vars are set.
	emptyEnv := filepath.Join(t.TempDir(), "empty.env")
	if err := os.WriteFile(emptyEnv, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("bash", "-c", `. "$1"; fleet_resolve_dsn FLEET_SCHED_DATABASE_URL "$2"`, "_", lib, emptyEnv)
	cmd.Env = append(os.Environ(), "DATABASE_URL=", "FLEET_SCHED_DATABASE_URL=", "SCHED_DATABASE_URL=", "DB_HOST=", "DB_PORT=", "DB_USER=", "DB_PASSWORD=", "DB_NAME=", "DB_SSLMODE=")
	out, err = cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fleet_resolve_dsn default: %v\n%s", err, string(out))
	}
	if got := strings.TrimSpace(string(out)); got != "postgres://fleet@localhost:5432/sched?sslmode=disable" {
		t.Errorf("fleet_resolve_dsn default got %q", got)
	}
	if got := idleSchedDSN(""); got == "" {
		t.Errorf("idleSchedDSN(\"\") = empty, want default DSN")
	}
	if got := idleChatDSN(""); got == "" {
		t.Errorf("idleChatDSN(\"\") = empty, want default DSN")
	}

	// Verify fractional systemd Unix timestamp parsing.
	if got := parseServiceTimestamp("@1793108488.662800"); got != 1793108488 {
		t.Errorf("parseServiceTimestamp(@1793108488.662800) = %d, want 1793108488", got)
	}
	if got := redactScheduledTaskTitle(`scheduled task 11111111-2222-3333-4444-555555555555 ("Customer ACME, Inc.", running, 14s)`); got != "scheduled task 11111111-2222-3333-4444-555555555555 (running, 14s)" {
		t.Errorf("redactScheduledTaskTitle = %q", got)
	}
}

func TestUpdateBlocksOnActiveWorkUnlessWaitOrForce(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stateFile := filepath.Join(tmp, "psql-calls")
	fakePsql := filepath.Join(binDir, "psql")
	// Fake psql: records call count in stateFile. When FLEET_TEST_BUSY_CALLS is
	// set, returns an active scheduled task for the first N calls, then empty.
	script := `#!/usr/bin/env bash
if [[ "$*" == *"to_regclass('public.tasks') IS NOT NULL"* ]]; then
  printf 't\n'
  exit 0
fi
if [[ "$*" == *"FROM tasks"* ]]; then
  count=0
  [[ -f "` + stateFile + `" ]] && count="$(cat "` + stateFile + `")"
  count=$(( count + 1 ))
  printf '%s\n' "$count" > "` + stateFile + `"
  busy_Limit="${FLEET_TEST_BUSY_CALLS:-999}"
  if (( count <= busy_Limit )); then
    printf 'scheduled task 11111111-2222-3333-4444-555555555555 (Hourly Sync, running, 14s)\n'
  fi
fi
exit 0
`
	if err := os.WriteFile(fakePsql, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(tmp, "fleet.env")
	if err := os.WriteFile(envFile, []byte("FLEET_SCHED_DATABASE_URL=postgres://sched:pw@127.0.0.1:5432/sched?sslmode=disable\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseEnv := []string{
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
		"FLEET_ENV_FILE=" + envFile,
		"FLEET_ASSUME_SERVICE_ACTIVE=1",
	}

	// 1. Dry-run reports the blocker and marks the plan blocked.
	out, err := runScript(t, baseEnv, "update.sh", "--dry-run", "--no-pull")
	if err != nil {
		t.Fatalf("dry-run must exit 0 even when blocked: %v\n%s", err, out)
	}
	for _, want := range []string{
		"BLOCKER: active work in flight",
		"Hourly Sync",
		"dry run: plan printed, and a BLOCKER above would stop the real run",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}

	// 2. Real run without --wait or --force refuses before step 1 and cleans up the drain file.
	_ = os.Remove(stateFile)
	out, err = runScript(t, baseEnv, "update.sh", "--no-pull")
	if err == nil {
		t.Fatalf("expected update.sh to refuse when a task is running\n%s", out)
	}
	if !strings.Contains(out, "refusing to update while work is in flight") || !strings.Contains(out, "Hourly Sync") {
		t.Errorf("unexpected refusal output:\n%s", out)
	}
	if strings.Contains(out, "1/5  Updating the fleet checkout") {
		t.Errorf("update must refuse before step 1:\n%s", out)
	}
	if _, statErr := os.Stat(envFile + ".update.drain"); !os.IsNotExist(statErr) {
		t.Errorf("drain file must be removed when update is refused, got statErr=%v", statErr)
	}

	// 3. Dry-run with --force skips the blocker.
	_ = os.Remove(stateFile)
	out, err = runScript(t, baseEnv, "update.sh", "--dry-run", "--no-pull", "--force")
	if err != nil {
		t.Fatalf("dry-run --force failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "--force set: skipping the active-work blocker") {
		t.Errorf("expected --force notice in dry-run:\n%s", out)
	}
	if strings.Contains(out, "BLOCKER: active work in flight") {
		t.Errorf("--force must not report active-work blocker:\n%s", out)
	}

	// 4. fleet_wait_for_idle invoked inside a $(...) subshell leaves the drain
	// file set on success, and fleet_clear_drain in the parent shell removes it.
	_ = os.Remove(stateFile)
	root := repoRootFromTest(t)
	idleLib := filepath.Join(root, "scripts", "lib", "idle.sh")
	cmd := exec.Command("bash", "-c", `. "$1"; _summary="$(fleet_wait_for_idle "$2" 5 1)"; [[ -f "$2.update.drain" ]] || exit 2; fleet_clear_drain "$2"; [[ ! -f "$2.update.drain" ]]`, "_", idleLib, envFile)
	cmd.Env = append(append(os.Environ(), baseEnv...), "FLEET_TEST_BUSY_CALLS=1")
	if waitOut, werr := cmd.CombinedOutput(); werr != nil {
		t.Fatalf("fleet_wait_for_idle subshell + parent fleet_clear_drain failed: %v\n%s", werr, string(waitOut))
	}
}

func TestActiveWorkAndWaitIdle_DB(t *testing.T) {
	database := db.New()
	if err := database.Init("", db.DefaultPoolConfig()); err != nil {
		t.Skipf("sched DB unavailable: %v", err)
	}
	ctx := context.Background()
	conn, err := database.Conn().Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(6)"); err != nil {
		_ = conn.Close()
		t.Fatalf("lock: %v", err)
	}
	clean := func() {
		for _, q := range []string{"DELETE FROM logs", "DELETE FROM tasks", "DELETE FROM users"} {
			_, _ = database.Conn().ExecContext(ctx, q)
		}
	}
	clean()
	t.Cleanup(func() {
		ClearUpdateDrain()
		clean()
		_, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock(6)")
		_ = conn.Close()
		database.Close()
	})

	drainFile := filepath.Join(t.TempDir(), "fleet.update.drain")
	t.Setenv("FLEET_UPDATE_DRAIN_FILE", drainFile)
	t.Setenv("FLEET_ASSUME_SERVICE_ACTIVE", "1")

	dsn, err := schedDSN("")
	if err != nil || dsn == "" {
		t.Skip("sched DSN not set in environment")
	}

	items, err := ActiveWork(ctx, dsn, "")
	if err != nil || len(items) != 0 {
		t.Fatalf("expected idle DB, got items=%v err=%v", items, err)
	}

	st := storage.New()
	st.SetDatabase(database)

	// 1. A crash-stranded task with an expired lease must NOT block ActiveWork.
	expiredID := uuid.New()
	expiredLease := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Microsecond)
	expiredTask := &models.Task{
		ID:             expiredID,
		Title:          "Expired Lease Task",
		Prompt:         "stranded",
		Status:         models.TaskStatusRunning,
		CreatedAt:      time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Microsecond),
		LeaseExpiresAt: &expiredLease,
	}
	if _, err := st.AddTaskWithContext(ctx, expiredTask); err != nil {
		t.Fatalf("AddTask expired: %v", err)
	}
	items, err = ActiveWork(ctx, dsn, "")
	if err != nil || len(items) != 0 {
		t.Fatalf("expected expired lease task to be ignored, got items=%v err=%v", items, err)
	}

	// 2. A task with a live lease blocks ActiveWork and WaitIdle(0).
	taskID := uuid.New()
	liveLease := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond)
	runningTask := &models.Task{
		ID:             taskID,
		Title:          "Northwind Digest",
		Prompt:         "run digest",
		Status:         models.TaskStatusRunning,
		CreatedAt:      time.Now().UTC().Truncate(time.Microsecond),
		LeaseExpiresAt: &liveLease,
	}
	if _, err := st.AddTaskWithContext(ctx, runningTask); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	items, err = ActiveWork(ctx, dsn, "")
	if err != nil || len(items) != 1 || !strings.Contains(items[0], "Northwind Digest") {
		t.Fatalf("expected running task in ActiveWork, got items=%v err=%v", items, err)
	}

	if err := WaitIdle(ctx, dsn, "", 0, 10*time.Millisecond, nil); err == nil || !strings.Contains(err.Error(), "Northwind Digest") {
		t.Fatalf("expected WaitIdle(0) to fail with Northwind Digest, got %v", err)
	}
	if _, statErr := os.Stat(drainFile); !os.IsNotExist(statErr) {
		t.Fatalf("expected WaitIdle timeout to clear drain file, got statErr=%v", statErr)
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		_, _ = database.Conn().ExecContext(context.Background(), "UPDATE tasks SET status = 'success' WHERE id = $1", taskID)
	}()
	var waitLog bytes.Buffer
	if err := WaitIdle(ctx, dsn, "", 2*time.Second, 10*time.Millisecond, &waitLog); err != nil {
		t.Fatalf("expected WaitIdle to succeed after task completed: %v", err)
	}
	if _, statErr := os.Stat(drainFile); statErr != nil {
		t.Fatalf("expected WaitIdle success to leave drain file set for caller: %v", statErr)
	}
	ClearUpdateDrain()
}
