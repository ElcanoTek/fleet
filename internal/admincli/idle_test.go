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
			dsn:      "postgres://fleet_chat@127.0.0.1:5432/chat?sslmode=disable",
			wantPass: "",
			wantRed:  "postgres://fleet_chat@127.0.0.1:5432/chat?sslmode=disable",
		},
		{
			dsn:      "postgres://127.0.0.1:5432/chat",
			wantPass: "",
			wantRed:  "postgres://127.0.0.1:5432/chat",
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
count=0
[[ -f "` + stateFile + `" ]] && count="$(cat "` + stateFile + `")"
count=$(( count + 1 ))
printf '%s\n' "$count" > "` + stateFile + `"
busy_Limit="${FLEET_TEST_BUSY_CALLS:-999}"
if (( count <= busy_Limit )); then
  if [[ "$*" == *"FROM tasks"* ]]; then
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

	// 2. Real run without --wait or --force refuses before step 1.
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

	// 4. fleet_wait_for_idle polls until work clears.
	_ = os.Remove(stateFile)
	root := repoRootFromTest(t)
	idleLib := filepath.Join(root, "scripts", "lib", "idle.sh")
	cmd := exec.Command("bash", "-c", `. "$1"; fleet_wait_for_idle "$2" 5 1`, "_", idleLib, envFile)
	cmd.Env = append(append(os.Environ(), baseEnv...), "FLEET_TEST_BUSY_CALLS=1")
	if waitOut, werr := cmd.CombinedOutput(); werr != nil {
		t.Fatalf("fleet_wait_for_idle should succeed once work clears: %v\n%s", werr, string(waitOut))
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
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(1)"); err != nil {
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
		clean()
		_, _ = conn.ExecContext(ctx, "SELECT pg_advisory_unlock(1)")
		_ = conn.Close()
		database.Close()
	})

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
	taskID := uuid.New()
	runningTask := &models.Task{
		ID:        taskID,
		Title:     "Northwind Digest",
		Prompt:    "run digest",
		Status:    models.TaskStatusRunning,
		CreatedAt: time.Now().UTC().Truncate(time.Microsecond),
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

	go func() {
		time.Sleep(30 * time.Millisecond)
		_, _ = database.Conn().ExecContext(context.Background(), "UPDATE tasks SET status = 'success' WHERE id = $1", taskID)
	}()
	var waitLog bytes.Buffer
	if err := WaitIdle(ctx, dsn, "", 2*time.Second, 10*time.Millisecond, &waitLog); err != nil {
		t.Fatalf("expected WaitIdle to succeed after task completed: %v", err)
	}
}
