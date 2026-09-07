package admincli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// fakeCleanupHost scripts which tools exist and which steps fail, recording
// every step runCleanup asked for.
type fakeCleanupHost struct {
	tools map[string]bool
	fail  map[string]bool // keyed by "name arg arg…"
	ran   []string
	out   bytes.Buffer
}

func (f *fakeCleanupHost) host() cleanupHost {
	return cleanupHost{
		lookPath: func(name string) (string, error) {
			if f.tools[name] {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
		run: func(name string, args ...string) error {
			key := strings.Join(append([]string{name}, args...), " ")
			f.ran = append(f.ran, key)
			if f.fail[key] {
				return errors.New("step failed")
			}
			return nil
		},
		out: &f.out,
	}
}

// TestRunCleanupExitContract — `fleet cleanup` used to exit 0 no matter what,
// so the maintenance timer's unit could never see a sweep that reclaimed
// nothing. Now: 5 when EVERY prune step that ran failed; 0 when at least one
// succeeded, when no step applied (tools absent), and on --dry-run (which
// never prunes, so it has nothing to fail).
func TestRunCleanupExitContract(t *testing.T) {
	const (
		imagePrune  = "podman image prune -f"
		systemPrune = "podman system prune -f"
		goClean     = "go clean -cache -testcache"
	)
	cases := []struct {
		name  string
		opts  cleanupOpts
		tools map[string]bool
		fail  map[string]bool
		want  int
		ran   []string
	}{
		{"every step fails → 5", cleanupOpts{}, map[string]bool{"podman": true, "go": true},
			map[string]bool{imagePrune: true, goClean: true}, 5, []string{imagePrune, goClean}},
		{"podman fails, go cleans → 0 (partial is still a sweep)", cleanupOpts{}, map[string]bool{"podman": true, "go": true},
			map[string]bool{imagePrune: true}, 0, []string{imagePrune, goClean}},
		{"only podman, and it fails → 5", cleanupOpts{}, map[string]bool{"podman": true},
			map[string]bool{imagePrune: true}, 5, []string{imagePrune}},
		{"--deep runs system prune; one success → 0", cleanupOpts{deep: true}, map[string]bool{"podman": true},
			map[string]bool{imagePrune: true}, 0, []string{imagePrune, systemPrune}},
		{"--deep, both prunes fail → 5", cleanupOpts{deep: true}, map[string]bool{"podman": true},
			map[string]bool{imagePrune: true, systemPrune: true}, 5, []string{imagePrune, systemPrune}},
		{"no tools → nothing applies → 0", cleanupOpts{}, nil, nil, 0, nil},
		{"--dry-run never prunes, so a failing df is still 0", cleanupOpts{dryRun: true, deep: true}, map[string]bool{"podman": true, "go": true},
			map[string]bool{"podman system df": true}, 0, []string{"podman system df"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCleanupHost{tools: tc.tools, fail: tc.fail}
			if got := runCleanup(tc.opts, f.host()); got != tc.want {
				t.Errorf("exit %d, want %d\n%s", got, tc.want, f.out.String())
			}
			if strings.Join(f.ran, "|") != strings.Join(tc.ran, "|") {
				t.Errorf("steps run = %q, want %q", f.ran, tc.ran)
			}
			if tc.opts.dryRun && !strings.Contains(f.out.String(), "[dry-run] would run: podman image prune -f") {
				t.Errorf("dry-run should announce the prune it would run:\n%s", f.out.String())
			}
		})
	}
}
