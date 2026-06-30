package admincli

import (
	"os/exec"
	"slices"
	"testing"
)

func TestLogsArgs(t *testing.T) {
	cases := []struct {
		name   string
		unit   string
		lines  int
		follow bool
		want   []string
	}{
		{"default", "fleet.service", 50, false, []string{"-u", "fleet.service", "-n", "50"}},
		{"custom n", "fleet.service", 200, false, []string{"-u", "fleet.service", "-n", "200"}},
		{"follow appends -f", "fleet.service", 50, true, []string{"-u", "fleet.service", "-n", "50", "-f"}},
		{"custom unit", "custom.service", 10, true, []string{"-u", "custom.service", "-n", "10", "-f"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := logsArgs(c.unit, c.lines, c.follow); !slices.Equal(got, c.want) {
				t.Errorf("logsArgs(%q,%d,%v) = %v, want %v", c.unit, c.lines, c.follow, got, c.want)
			}
		})
	}
}

// TestServiceVerbsRequireSystemctl: in an environment without systemctl on PATH,
// restart/stop return a non-zero usage code rather than panicking. (CI has no
// installed fleet.service unit, so we don't exercise the happy exec path here.)
func TestServiceVerbsRequireSystemctl(t *testing.T) {
	if _, err := exec.LookPath("systemctl"); err == nil {
		t.Skip("systemctl present; skipping the no-systemd path")
	}
	if code := cmdRestart(nil); code == 0 {
		t.Error("restart with no systemctl must return non-zero")
	}
	if code := cmdStop(nil); code == 0 {
		t.Error("stop with no systemctl must return non-zero")
	}
}

// TestServiceVerbsRejectBadFlags: an unknown flag is a parse error → exit 1,
// matching the cmdStatus convention (flag.ContinueOnError → return 1). Covers all
// three verbs without execing anything.
func TestServiceVerbsRejectBadFlags(t *testing.T) {
	for _, fn := range []struct {
		name string
		run  func([]string) int
	}{
		{"restart", cmdRestart},
		{"stop", cmdStop},
		{"logs", cmdLogs},
	} {
		if code := fn.run([]string{"--definitely-not-a-flag"}); code != 1 {
			t.Errorf("%s with a bad flag = %d, want 1", fn.name, code)
		}
	}
}

// TestServiceVerbsWiredInDispatch: the restart/stop/logs/tail verbs route through
// Run (not "unknown command"). We assert the dispatched code matches calling
// the verb directly — i.e. the wiring exists — using a bad flag so no real
// systemctl/journalctl runs.
func TestServiceVerbsWiredInDispatch(t *testing.T) {
	for _, verb := range []string{"restart", "stop", "logs", "tail"} {
		if code := Run([]string{verb, "--definitely-not-a-flag"}); code != 1 {
			t.Errorf("Run(%q --bad) = %d, want 1 (verb must be wired, parse-error exit 1)", verb, code)
		}
	}
}
