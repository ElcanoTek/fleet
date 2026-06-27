package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

func TestRetentionDays(t *testing.T) {
	t.Setenv("FLEET_BACKUP_RETENTION_DAYS", "7")
	if got := retentionDays(); got != 7 {
		t.Errorf("env: got %d, want 7", got)
	}
	t.Setenv("FLEET_BACKUP_RETENTION_DAYS", "garbage")
	if got := retentionDays(); got != 30 {
		t.Errorf("invalid env falls back: got %d, want 30", got)
	}
	t.Setenv("FLEET_BACKUP_RETENTION_DAYS", "")
	if got := retentionDays(); got != 30 {
		t.Errorf("default: got %d, want 30", got)
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
