package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakePrunePodman writes a shell script that stands in for podman: `ps` prints
// psOutput, `rm` records its arguments to rm.args. Returns the script path and
// the rm.args path.
func fakePrunePodman(t *testing.T, psOutput string) (script, rmArgs string) {
	t.Helper()
	dir := t.TempDir()
	psFile := filepath.Join(dir, "ps.out")
	if err := os.WriteFile(psFile, []byte(psOutput), 0o600); err != nil {
		t.Fatalf("write ps.out: %v", err)
	}
	rmArgs = filepath.Join(dir, "rm.args")
	script = filepath.Join(dir, "podman")
	body := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  ps) cat '" + psFile + "' ;;\n" +
		"  rm) shift; echo \"$@\" > '" + rmArgs + "' ;;\n" +
		"  *) exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake podman: %v", err)
	}
	return script, rmArgs
}

// TestPruneOrphanedContainers_ScopedByInstanceLabel verifies the ownership
// scoping: the boot-time sweep must reclaim genuinely orphaned containers
// (exited, or running with a dead owner pid in the fleet.instance label) while
// NEVER rm -f'ing a running container owned by a live fleet process — or one
// whose ownership cannot be established at all. Before the fix the sweep
// removed everything matching the name prefix, killing a sibling instance's
// live sandboxes on every boot of a shared podman user.
func TestPruneOrphanedContainers_ScopedByInstanceLabel(t *testing.T) {
	const livePID = "31337"
	psOutput := strings.Join([]string{
		"aaa|running|4242@100",             // owner pid dead → crashed prior run → remove
		"bbb|running|" + thisInstanceLabel, // this instance's own container → skip
		"ccc|exited|4242@100",              // not running → always remove
		"ddd|running|",                     // unlabeled (pre-label build) but running → skip
		"eee|running|" + livePID + "@50",   // live sibling instance → skip
		"fff|created|",                     // not running → always remove
		"ggg|running|not-a-label",          // malformed label, can't attribute → skip
		// The warm-pool race: this process's OWN containers can legitimately be
		// in "created" state while the sweep lists them, because the sweep runs
		// after buildInteractiveEngine has started the pool filling. Before the
		// own-label check these two were force-removed by their own process at
		// every boot, since the created/exited branch ignored the label.
		"hhh|created|" + thisInstanceLabel,
		"iii|exited|" + thisInstanceLabel,
	}, "\n") + "\n"
	script, rmArgs := fakePrunePodman(t, psOutput)

	origPidAlive := pidAlive
	t.Cleanup(func() { pidAlive = origPidAlive })
	pidAlive = func(pid int) bool {
		return pid == os.Getpid() || pid == 31337
	}

	removed, err := PruneOrphanedContainers(context.Background(), script)
	if err != nil {
		t.Fatalf("PruneOrphanedContainers: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed = %d, want 3 (this process's own created/exited containers must NOT be swept)", removed)
	}
	got, err := os.ReadFile(rmArgs)
	if err != nil {
		t.Fatalf("read recorded rm args: %v", err)
	}
	if want := "-f aaa ccc fff"; strings.TrimSpace(string(got)) != want {
		t.Errorf("rm args = %q, want %q", strings.TrimSpace(string(got)), want)
	}
}

// TestPruneOrphanedContainers_NothingToPrune covers the boring boot: no
// leftover containers means zero removals, no error, and no `podman rm` call.
func TestPruneOrphanedContainers_NothingToPrune(t *testing.T) {
	script, rmArgs := fakePrunePodman(t, "\n")

	removed, err := PruneOrphanedContainers(context.Background(), script)
	if err != nil {
		t.Fatalf("PruneOrphanedContainers: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
	if _, err := os.Stat(rmArgs); !os.IsNotExist(err) {
		t.Errorf("podman rm must not run when there is nothing to prune (stat err = %v)", err)
	}
}

func TestInstanceLabelPID(t *testing.T) {
	cases := []struct {
		label   string
		pid     int
		started int64
		ok      bool
	}{
		{"1234@567", 1234, 567, true},
		{"", 0, 0, false},
		{"no-at-sign", 0, 0, false},
		{"abc@567", 0, 0, false},
		{"-5@567", 0, 0, false},
		{"0@567", 0, 0, false},
		// A label with an unusable start half still yields the pid, with
		// startedAt 0 meaning "unverifiable" — NOT "started at the epoch",
		// which would make every such owner look long dead.
		{"1234@", 1234, 0, true},
		{"1234@notanumber", 1234, 0, true},
		{"1234@0", 1234, 0, true},
		{"1234@-9", 1234, 0, true},
	}
	for _, tc := range cases {
		pid, started, ok := instanceLabelOwner(tc.label)
		if pid != tc.pid || started != tc.started || ok != tc.ok {
			t.Errorf("instanceLabelOwner(%q) = (%d, %d, %v), want (%d, %d, %v)", tc.label, pid, started, ok, tc.pid, tc.started, tc.ok)
		}
	}
}

// TestLabeledOwnerStillRunningPIDReuse is the starvation guard. Before the
// start-time check, a crashed run's container was skipped forever once its pid
// was REUSED by any unrelated live process — the orphan the sweep exists to
// reclaim became immortal, while prune.go's own comment claimed the label's
// start time "disambiguates pid reuse". Nothing read it.
func TestLabeledOwnerStillRunningPIDReuse(t *testing.T) {
	origAlive, origStart := pidAlive, pidStartedAtUnix
	t.Cleanup(func() { pidAlive, pidStartedAtUnix = origAlive, origStart })

	cases := []struct {
		name         string
		alive        bool
		labeledStart int64
		actualStart  int64
		actualOK     bool
		want         bool
	}{
		{"dead pid is not running", false, 1000, 0, false, false},
		{"same process (identical start)", true, 1000, 1000, true, true},
		{"same process (start just before its label)", true, 1000, 998, true, true},
		{"same process (clock skew inside tolerance)", true, 1000, 1000 + pidReuseTolerance - 1, true, true},
		// The bug: pid alive but it started long after the label was stamped,
		// so it is a DIFFERENT process wearing a recycled pid.
		{"reused pid started well after the label", true, 1000, 1000 + pidReuseTolerance + 1, true, false},
		{"reused pid started hours later", true, 1000, 1000 + 86400, true, false},
		// Unknowns fail SAFE: never remove a container we cannot prove stale.
		{"unverifiable /proc read assumes alive", true, 1000, 0, false, true},
		{"label without a start half assumes alive", true, 0, 1000 + 86400, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pidAlive = func(int) bool { return tc.alive }
			pidStartedAtUnix = func(int) (int64, bool) { return tc.actualStart, tc.actualOK }
			if got := labeledOwnerStillRunning(4242, tc.labeledStart); got != tc.want {
				t.Errorf("labeledOwnerStillRunning = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPIDStartedAtUnixRealProcess exercises the real /proc reader against this
// test process and pid 1, so the field offset and the USER_HZ arithmetic are
// pinned against the running kernel rather than a fixture.
func TestPIDStartedAtUnixRealProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc-based start time is linux-only")
	}
	self, ok := pidStartedAtUnix(os.Getpid())
	if !ok {
		t.Fatal("could not read this process's start time from /proc")
	}
	now := time.Now().Unix()
	// This test binary started moments ago; anything outside a wide window means
	// the field offset or the tick conversion is wrong.
	if self > now+5 || self < now-3600 {
		t.Errorf("own start time = %d, which is not plausibly within the last hour of now=%d", self, now)
	}
	init, ok := pidStartedAtUnix(1)
	if !ok {
		t.Fatal("could not read pid 1's start time")
	}
	if init > self {
		t.Errorf("pid 1 start (%d) is after this process's start (%d) — the arithmetic is wrong", init, self)
	}
	if _, ok := pidStartedAtUnix(-1); ok {
		t.Error("a negative pid must not resolve a start time")
	}
}
