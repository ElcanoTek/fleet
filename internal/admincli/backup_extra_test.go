package admincli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupFilePattern(t *testing.T) {
	match := []string{"fleet-chat-20260627T080000Z.dump", "fleet-sched-20260101T000000Z.dump"}
	noMatch := []string{"fleet-other-x.dump", "notes.txt", "fleet-chat.sql", "chat-20260627.dump"}
	for _, n := range match {
		if !backupFilePattern.MatchString(n) {
			t.Errorf("expected %q to match backup pattern", n)
		}
	}
	for _, n := range noMatch {
		if backupFilePattern.MatchString(n) {
			t.Errorf("expected %q NOT to match backup pattern", n)
		}
	}
}

func TestPruneOldBackups(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().AddDate(0, 0, -40)
	recent := time.Now()

	write := func(name string, mod time.Time) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	write("fleet-chat-old.dump", old)       // prune
	write("fleet-sched-old.dump", old)      // prune
	write("fleet-chat-recent.dump", recent) // keep (too new)
	write("unrelated-old.txt", old)         // keep (not a backup file)

	n, err := pruneOldBackups(dir, 30)
	if err != nil {
		t.Fatalf("pruneOldBackups: %v", err)
	}
	if n != 2 {
		t.Errorf("pruned %d, want 2", n)
	}
	for _, kept := range []string{"fleet-chat-recent.dump", "unrelated-old.txt"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s should have been kept: %v", kept, err)
		}
	}
	for _, gone := range []string{"fleet-chat-old.dump", "fleet-sched-old.dump"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should have been pruned", gone)
		}
	}
}

func TestBackupDir(t *testing.T) {
	t.Setenv("FLEET_BACKUP_DIR", "/env/backups")
	if got := backupDir("/flag/dir"); got != "/flag/dir" {
		t.Errorf("flag should win: got %q", got)
	}
	if got := backupDir(""); got != "/env/backups" {
		t.Errorf("env fallback: got %q", got)
	}
	t.Setenv("FLEET_BACKUP_DIR", "")
	if got := backupDir(""); got != "." {
		t.Errorf("default: got %q, want .", got)
	}
}

// TestBackupDirReadsEnvFile — `fleet backup` must honor a FLEET_BACKUP_DIR set
// only in the deployment env file, exactly as the timer unit (and
// resolveBackupDir) does; it used to read the process env only, so a hand-run
// backup on a provisioned box dumped into the cwd while the timer used the
// configured directory.
func TestBackupDirReadsEnvFile(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(envFile, []byte("FLEET_BACKUP_DIR=/mnt/from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_ENV_FILE", envFile)
	t.Setenv("FLEET_BACKUP_DIR", "")
	resetEnvFileCache()
	t.Cleanup(resetEnvFileCache)
	if got := backupDir(""); got != "/mnt/from-file" {
		t.Errorf("env-file fallback: got %q, want /mnt/from-file", got)
	}
	if got := resolveBackupDir(); got != "/mnt/from-file" {
		t.Errorf("timer resolution must agree: got %q", got)
	}
	if got := backupDir("/flag"); got != "/flag" {
		t.Errorf("flag should still win: got %q", got)
	}
}

// TestEnsureBackupDirOwnerOnly — dumps are the whole database, so the
// directory that holds them is created 0700 (it used to be 0750).
func TestEnsureBackupDirOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "backups")
	if err := ensureBackupDir(dir); err != nil {
		t.Fatalf("ensureBackupDir: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("backup dir mode = %v, want 0700", perm)
	}
	// Idempotent on an existing dir.
	if err := ensureBackupDir(dir); err != nil {
		t.Errorf("second ensureBackupDir: %v", err)
	}
}

func TestRetentionDays(t *testing.T) {
	t.Setenv("FLEET_BACKUP_RETENTION_DAYS", "7")
	got, err := retentionDays()
	if err != nil || got != 7 {
		t.Errorf("env: got %d, %v, want 7, nil", got, err)
	}
	// #1273: a malformed or non-positive value is now an ERROR that refuses the
	// prune, not a silent fall back to the 30-day default — pruning backups off
	// a misread retention is not a recoverable mistake.
	for _, bad := range []string{"garbage", "0", "-1"} {
		t.Setenv("FLEET_BACKUP_RETENTION_DAYS", bad)
		got, err := retentionDays()
		if err == nil {
			t.Errorf("FLEET_BACKUP_RETENTION_DAYS=%q: want an error, got %d", bad, got)
			continue
		}
		if !strings.Contains(err.Error(), "FLEET_BACKUP_RETENTION_DAYS") {
			t.Errorf("error should name the variable, got: %v", err)
		}
		if got != 30 {
			t.Errorf("the returned fallback should stay the default 30, got %d", got)
		}
	}
	t.Setenv("FLEET_BACKUP_RETENTION_DAYS", "")
	got, err = retentionDays()
	if err != nil || got != 30 {
		t.Errorf("default: got %d, %v, want 30, nil", got, err)
	}
}

func TestVerifyDump_RejectsCorrupt(t *testing.T) {
	if _, err := exec.LookPath("pg_restore"); err != nil {
		t.Skip("pg_restore not in PATH — skipping integrity-check test")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "fleet-chat-corrupt.dump")
	if err := os.WriteFile(bad, []byte("this is not a pg custom-format archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyDump(context.Background(), bad); err == nil {
		t.Error("verifyDump should reject a non-archive file")
	}
}
