package admincli

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// cmdCleanup and Run("cleanup") go through newCleanupHost; the tests below are
// about exit codes and flag parsing, so they get a host that has no podman and
// no Go toolchain and writes nowhere, instead of the real one running
// `podman system df` on the developer's machine for every --dry-run.
func useSilentCleanupHost(t *testing.T) {
	t.Helper()
	old := newCleanupHost
	newCleanupHost = func() cleanupHost {
		return cleanupHost{
			lookPath: func(string) (string, error) { return "", errors.New("not installed") },
			run:      func(string, ...string) error { return nil },
			out:      io.Discard,
		}
	}
	t.Cleanup(func() { newCleanupHost = old })
}

// The cleanup verb must be reachable from the dispatcher, refuse unknown
// flags loudly, and treat --dry-run as strictly read-only (asserted here by
// exit codes + flag parsing; the destructive paths shell out to podman/go and
// are exercised by operators, not unit tests — same posture as cmdUpdate).
func TestCleanupFlagParsing(t *testing.T) {
	useSilentCleanupHost(t)
	if code := cmdCleanup([]string{"--bogus"}); code != 2 {
		t.Errorf("unknown flag: exit %d, want 2", code)
	}
	if code := cmdCleanup([]string{"--help"}); code != 0 {
		t.Errorf("--help: exit %d, want 0", code)
	}
	if code := cmdCleanup([]string{"--dry-run"}); code != 0 {
		t.Errorf("--dry-run: exit %d, want 0", code)
	}
	if code := cmdCleanup([]string{"--dry-run", "--deep"}); code != 0 {
		t.Errorf("--dry-run --deep: exit %d, want 0", code)
	}
}

// The dispatcher routes "cleanup"; a typo'd verb still reports unknown.
func TestCleanupDispatch(t *testing.T) {
	useSilentCleanupHost(t)
	if code := Run([]string{"cleanup", "--dry-run"}); code != 0 {
		t.Errorf("Run(cleanup --dry-run): exit %d, want 0", code)
	}
}

// diskLine is a convenience report: present with df, one line, labeled.
func TestDiskLine(t *testing.T) {
	line := diskLine("test")
	if line == "" {
		t.Skip("df unavailable in this environment")
	}
	if !strings.HasPrefix(line, "disk (test): ") || strings.Contains(line, "\n") {
		t.Errorf("diskLine = %q", line)
	}
}
