// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package boxdoctor

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
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

func TestRestartChurnVerdict(t *testing.T) {
	// The healthy case. NRestarts is cleared by any manual restart, so 0 is
	// what a freshly deployed, steadily serving unit reports.
	if c := restartChurnVerdict("fleet-web", "0", "success"); c.Status != StatusOK {
		t.Errorf("no restarts: got %s (%s), want ok", c.Status, c.Detail)
	}
	// A stale Result from before the last manual start must not by itself
	// demote a unit that is no longer restarting — but it should still be
	// visible in the detail, because it says what went wrong.
	c := restartChurnVerdict("fleet-web", "0", "core-dump")
	if c.Status != StatusOK {
		t.Errorf("no restarts with a stale bad Result: got %s, want ok", c.Status)
	}
	if !strings.Contains(c.Detail, "core-dump") {
		t.Errorf("detail %q should still surface the last Result", c.Detail)
	}
	// A couple of self-inflicted restarts: worth flagging, not yet a failure.
	c = restartChurnVerdict("fleet-web", "2", "signal")
	if c.Status != StatusWarn || c.Fix == "" {
		t.Errorf("2 restarts: got %s fix=%q, want warn with a fix", c.Status, c.Fix)
	}
	if !strings.Contains(c.Fix, "journalctl -u fleet-web") {
		t.Errorf("fix %q should point at the unit's journal", c.Fix)
	}
	// Crash-loop territory. This is the case checkUnits cannot see at all:
	// Restart=always keeps is-active reporting "active" throughout.
	c = restartChurnVerdict("fleet-web", "17", "core-dump")
	if c.Status != StatusFail || c.Fix == "" {
		t.Fatalf("17 restarts: got %s fix=%q, want fail with a fix", c.Status, c.Fix)
	}
	if !strings.Contains(c.Detail, "17") {
		t.Errorf("detail %q should name the restart count", c.Detail)
	}
	// The threshold boundary, both sides.
	if got := restartChurnVerdict("f", strconv.Itoa(crashLoopThreshold-1), "").Status; got != StatusWarn {
		t.Errorf("just below the threshold: got %s, want warn", got)
	}
	if got := restartChurnVerdict("f", strconv.Itoa(crashLoopThreshold), "").Status; got != StatusFail {
		t.Errorf("at the threshold: got %s, want fail", got)
	}
	// Unavailable or non-numeric property → skip, never a guessed verdict.
	for _, bad := range []string{"", "n/a", "[not set]"} {
		if got := restartChurnVerdict("f", bad, "").Status; got != StatusSkip {
			t.Errorf("NRestarts=%q: got %s, want skip", bad, got)
		}
	}
}

func TestWebStopPolicyVerdict(t *testing.T) {
	if c := webStopPolicyVerdict("kill"); c.Status != StatusOK {
		t.Errorf("kill: got %s (%s), want ok", c.Status, c.Detail)
	}
	// The regression this check exists for: the unit body said kill for a full
	// release while Fedora's global drop-in resolved abort, and every
	// file-comparing check passed. A warn with a fix is the point.
	c := webStopPolicyVerdict("abort")
	if c.Status != StatusWarn || c.Fix == "" {
		t.Fatalf("abort: got %s fix=%q, want warn with a fix", c.Status, c.Fix)
	}
	if !strings.Contains(c.Detail, "abort") {
		t.Errorf("detail %q should name the resolved value", c.Detail)
	}
	if !strings.Contains(c.Fix, "fleet doctor") {
		t.Errorf("fix %q should name the repair command", c.Fix)
	}
	// systemd's own default is no better than the distro's abort here.
	if got := webStopPolicyVerdict("terminate").Status; got != StatusWarn {
		t.Errorf("terminate: got %s, want warn", got)
	}
	// Pre-246 systemd: no such property, nothing to assert, no fix to offer.
	c = webStopPolicyVerdict("")
	if c.Status != StatusSkip {
		t.Errorf("unsupported systemd: got %s, want skip", c.Status)
	}
	if c.Fix != "" {
		t.Errorf("unsupported systemd should offer no fix, got %q", c.Fix)
	}
}

func TestRunIncludesShutdownChecks(t *testing.T) {
	r := Run(context.Background(), Options{DataDir: t.TempDir()})
	want := map[string]bool{"fleet-web stop policy": false}
	churn := 0
	for _, c := range r.Checks {
		if _, ok := want[c.Name]; ok {
			want[c.Name] = true
		}
		if strings.HasPrefix(c.Name, "restarts: ") {
			churn++
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("report has no %q check", name)
		}
	}
	// One per app unit (the configured service + fleet-web), or a single
	// systemctl-missing skip on a host without systemd — as in CI.
	if churn == 0 {
		t.Error("report has no restart-churn check")
	}
}
