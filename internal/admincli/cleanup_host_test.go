package admincli

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCleanupHost scripts which tools exist, which steps fail and whether
// podman's runtime dir is missing, recording every step runCleanup asked for.
type fakeCleanupHost struct {
	tools          map[string]bool
	fail           map[string]bool // keyed by "name arg arg…"
	runtimeMissing string          // non-empty: this runtime dir does not exist
	ran            []string
	out            bytes.Buffer
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
		missingRuntimeDir: func() (string, bool) {
			return f.runtimeMissing, f.runtimeMissing != ""
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
	t.Parallel()

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

// TestRunCleanupSkipsWhenRuntimeDirMissing pins the fresh-box case: the
// maintenance unit fired while fleet.service had never started, so /run/fleet
// (its RuntimeDirectory) did not exist and podman could only fail with
// "lstat /run/fleet". That is a skip with a clear line and exit 0, not a
// failed unit; podman is never invoked, and the Go cache step still runs.
func TestRunCleanupSkipsWhenRuntimeDirMissing(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		opts  cleanupOpts
		tools map[string]bool
		ran   []string
	}{
		{"plain run", cleanupOpts{}, map[string]bool{"podman": true}, nil},
		{"--deep", cleanupOpts{deep: true}, map[string]bool{"podman": true}, nil},
		{"--dry-run does not call podman system df", cleanupOpts{dryRun: true}, map[string]bool{"podman": true}, nil},
		{"go cache still cleaned", cleanupOpts{}, map[string]bool{"podman": true, "go": true}, []string{"go clean -cache -testcache"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCleanupHost{tools: tc.tools, runtimeMissing: "/run/fleet"}
			if got := runCleanup(tc.opts, f.host()); got != 0 {
				t.Errorf("exit %d, want 0 (a skip is not a failure)\n%s", got, f.out.String())
			}
			if strings.Join(f.ran, "|") != strings.Join(tc.ran, "|") {
				t.Errorf("steps run = %q, want %q", f.ran, tc.ran)
			}
			if !strings.Contains(f.out.String(), "podman runtime dir /run/fleet does not exist") ||
				!strings.Contains(f.out.String(), "Skipping image cleanup") {
				t.Errorf("want a clear skip line naming the dir:\n%s", f.out.String())
			}
		})
	}
}

// A real prune failure with the runtime dir present must still fail the unit:
// the skip covers only "the service is not running", not podman errors.
func TestRunCleanupRuntimeDirPresentStillFails(t *testing.T) {
	t.Parallel()

	f := &fakeCleanupHost{
		tools: map[string]bool{"podman": true},
		fail:  map[string]bool{"podman image prune -f": true},
	}
	if got := runCleanup(cleanupOpts{}, f.host()); got != 5 {
		t.Errorf("exit %d, want 5\n%s", got, f.out.String())
	}
	if strings.Contains(f.out.String(), "Skipping image cleanup") {
		t.Errorf("present runtime dir must not be reported as a skip:\n%s", f.out.String())
	}
}

// missingXDGRuntimeDir reads the real environment: unset is "not missing"
// (podman picks its own default), an existing dir is not missing, and only a
// dir that does not exist is.
func TestMissingXDGRuntimeDir(t *testing.T) {
	existing := t.TempDir()
	absent := filepath.Join(existing, "fleet")
	cases := []struct {
		name, env   string
		wantMissing bool
	}{
		{"unset", "", false},
		{"exists", existing, false},
		{"absent", absent, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_RUNTIME_DIR", tc.env)
			dir, missing := missingXDGRuntimeDir()
			if missing != tc.wantMissing {
				t.Errorf("missing = %v, want %v (dir %q)", missing, tc.wantMissing, dir)
			}
			if missing && dir != tc.env {
				t.Errorf("dir = %q, want %q", dir, tc.env)
			}
		})
	}
}
