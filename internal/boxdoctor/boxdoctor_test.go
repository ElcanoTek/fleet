// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package boxdoctor

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckModelKey(t *testing.T) {
	t.Setenv("FLEET_MOCK_MODE", "")
	t.Setenv("CHAT_MOCK_MODE", "")

	t.Setenv("OPENROUTER_API_KEY", "sk-test-placeholder")
	if c := checkModelKey(); c.Status != StatusOK {
		t.Errorf("key set: got %s (%s), want ok", c.Status, c.Detail)
	}

	t.Setenv("OPENROUTER_API_KEY", "")
	if c := checkModelKey(); c.Status != StatusFail || c.Fix == "" {
		t.Errorf("key unset: got %s fix=%q, want fail with a fix", c.Status, c.Fix)
	}

	t.Setenv("FLEET_MOCK_MODE", "1")
	if c := checkModelKey(); c.Status != StatusWarn {
		t.Errorf("mock mode: got %s, want warn", c.Status)
	}
}

func TestCheckSubIDs(t *testing.T) {
	dir := t.TempDir()

	// A file with the current user's range → ok.
	me := currentUsername(t)
	withRange := filepath.Join(dir, "subuid")
	if err := os.WriteFile(withRange, []byte("nobody:1:1\n"+me+":100000:65536\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := checkSubIDs("subuid", withRange); c.Status != StatusOK {
		t.Errorf("with range: got %s (%s), want ok", c.Status, c.Detail)
	}

	// A file without it → fail with the doctor fix.
	without := filepath.Join(dir, "subuid-empty")
	if err := os.WriteFile(without, []byte("nobody:1:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := checkSubIDs("subuid", without); c.Status != StatusFail || c.Fix == "" {
		t.Errorf("without range: got %s fix=%q, want fail with a fix", c.Status, c.Fix)
	}

	// Unreadable path → skip, never a false fail.
	if c := checkSubIDs("subuid", filepath.Join(dir, "absent")); c.Status != StatusSkip {
		t.Errorf("absent file: got %s, want skip", c.Status)
	}
}

func TestCheckDisk(t *testing.T) {
	if c := checkDisk("disk: t", t.TempDir()); c.Status != StatusOK && c.Status != StatusWarn && c.Status != StatusFail {
		t.Errorf("tempdir statfs: got %s (%s), want a real verdict", c.Status, c.Detail)
	}
	if c := checkDisk("disk: t", ""); c.Status != StatusSkip {
		t.Errorf("empty path: got %s, want skip", c.Status)
	}
	if c := checkDisk("disk: t", filepath.Join(t.TempDir(), "nope")); c.Status != StatusSkip {
		t.Errorf("missing path: got %s, want skip", c.Status)
	}
}

func TestCheckDBs(t *testing.T) {
	ctx := context.Background()

	if c := checkChatDB(ctx, nil); c.Status != StatusSkip {
		t.Errorf("nil ping: got %s, want skip", c.Status)
	}
	if c := checkChatDB(ctx, func(context.Context) error { return nil }); c.Status != StatusOK {
		t.Errorf("ok ping: got %s, want ok", c.Status)
	}
	if c := checkChatDB(ctx, func(context.Context) error { return errors.New("boom") }); c.Status != StatusFail || c.Fix == "" {
		t.Errorf("failing ping: got %s fix=%q, want fail with a fix", c.Status, c.Fix)
	}

	t.Setenv("FLEET_SCHED_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")
	if c := checkSchedDB(ctx, ""); c.Status != StatusFail {
		t.Errorf("no DSN: got %s (%s), want fail", c.Status, c.Detail)
	}
}

// TestBackupVerdict — #966: a box with no backup mechanism must be visible but
// must NOT fail the box (backing up at the volume/hypervisor layer is a valid
// answer), while a timer whose last run failed must fail it.
func TestBackupVerdict(t *testing.T) {
	if c := backupVerdict(false, false, false, ""); c.Status != StatusWarn || c.Fix == "" {
		t.Errorf("no timer: got %s fix=%q, want warn with a fix", c.Status, c.Fix)
	}
	// The absent-pair fix must name the one-command install verb, not a
	// copy-paste install/daemon-reload/enable chain.
	if c := backupVerdict(false, false, false, ""); !strings.Contains(c.Fix, "fleet timers install") {
		t.Errorf("no timer fix %q should name `fleet timers install`", c.Fix)
	}
	if c := backupVerdict(true, false, false, ""); c.Status != StatusWarn || c.Fix == "" {
		t.Errorf("timer installed but disabled: got %s fix=%q, want warn with a fix", c.Status, c.Fix)
	}
	// Enabled but stopped: nothing fires, and the service's Result still reads
	// "success", so this must not report ok.
	if c := backupVerdict(true, true, false, "success"); c.Status != StatusWarn || c.Fix == "" {
		t.Errorf("timer enabled but inactive: got %s fix=%q, want warn with a fix", c.Status, c.Fix)
	}
	if c := backupVerdict(true, true, true, "success"); c.Status != StatusOK {
		t.Errorf("healthy timer: got %s (%s), want ok", c.Status, c.Detail)
	}
	// A never-run oneshot also reports Result=success; systemd leaves the
	// property empty only when the unit is unknown.
	if c := backupVerdict(true, true, true, ""); c.Status != StatusOK {
		t.Errorf("never-run timer: got %s (%s), want ok", c.Status, c.Detail)
	}
	c := backupVerdict(true, true, true, "exit-code")
	if c.Status != StatusFail || c.Fix == "" {
		t.Fatalf("failed last run: got %s fix=%q, want fail with a fix", c.Status, c.Fix)
	}
	if !strings.Contains(c.Detail, "exit-code") {
		t.Errorf("failed last run detail %q should name the systemd Result", c.Detail)
	}
}

func TestRunIncludesBackupCheck(t *testing.T) {
	r := Run(context.Background(), Options{DataDir: t.TempDir()})
	for _, c := range r.Checks {
		if c.Name == "scheduled backups" {
			return
		}
	}
	t.Error("report has no scheduled-backups check")
}

func TestRunSummarizes(t *testing.T) {
	t.Setenv("FLEET_MOCK_MODE", "1") // model-key check → warn, not fail
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("FLEET_SCHED_DATABASE_URL", "")
	t.Setenv("DATABASE_URL", "")

	r := Run(context.Background(), Options{
		ChatPing: func(context.Context) error { return nil },
		DataDir:  t.TempDir(),
	})
	if len(r.Checks) == 0 {
		t.Fatal("no checks ran")
	}
	var tally Summary
	for _, c := range r.Checks {
		switch c.Status {
		case StatusOK:
			tally.OK++
		case StatusWarn:
			tally.Warn++
		case StatusFail:
			tally.Fail++
		case StatusSkip:
			tally.Skip++
		}
	}
	if tally != r.Summary {
		t.Errorf("summary %+v does not match checks %+v", r.Summary, tally)
	}
	// The empty sched DSN above guarantees at least one fail → unhealthy.
	if r.Healthy {
		t.Error("expected unhealthy (sched DSN unset)")
	}
	if r.Summary.Fail == 0 {
		t.Error("expected at least one fail")
	}
	if r.GeneratedAt.IsZero() {
		t.Error("GeneratedAt unset")
	}
}

func TestHelpers(t *testing.T) {
	t.Setenv("FLEET_SERVICE_NAME", "")
	if got := serviceName(""); got != "fleet" {
		t.Errorf("serviceName default = %q", got)
	}
	if got := serviceName(" custom "); got != "custom" {
		t.Errorf("serviceName flag = %q", got)
	}
	t.Setenv("FLEET_SANDBOX_IMAGE", "localhost/x:1")
	if got := resolveImageRef(""); got != "localhost/x:1" {
		t.Errorf("resolveImageRef env = %q", got)
	}
	if got := resolveImageRef("explicit:2"); got != "explicit:2" {
		t.Errorf("resolveImageRef explicit = %q", got)
	}
	if got := humanBytes(1536 * 1024 * 1024); got != "1.5 GiB" {
		t.Errorf("humanBytes = %q", got)
	}
	if got := humanBytes(512); got != "512 B" {
		t.Errorf("humanBytes small = %q", got)
	}
}

func currentUsername(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Skipf("current user unresolved: %v", err)
	}
	return u.Username
}
