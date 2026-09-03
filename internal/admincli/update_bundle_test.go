package admincli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitInit builds a throwaway repo with one commit and returns its path.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("mcp_servers: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "--quiet", "-m", "seed")
}

func capture(t *testing.T, fn func() bool) (string, bool) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	stale := fn()
	os.Stdout = orig
	_ = w.Close()
	buf := make([]byte, 8192)
	n, _ := r.Read(buf)
	_ = r.Close()
	return string(buf[:n]), stale
}

// TestClientBundleCheck covers the states an operator's box actually lands in.
// The one that motivated this is the last: a bundle with no upstream never
// fast-forwards, so `fleet update` leaves it behind while fleet itself advances
// — which surfaces in the UI as stale connector copy and nowhere else.
func TestClientBundleCheck(t *testing.T) {
	t.Run("no bundle configured", func(t *testing.T) {
		t.Setenv("FLEET_CLIENT_CONFIG_DIR", "")
		t.Setenv("FLEET_STATE_DIR", t.TempDir())
		out, stale := capture(t, clientBundleCheck)
		if stale {
			t.Error("a generic install has no bundle to be stale")
		}
		if !strings.Contains(out, "none configured") {
			t.Errorf("want the generic-bundle note, got %q", out)
		}
	})

	t.Run("not a git checkout", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("FLEET_CLIENT_CONFIG_DIR", dir)
		out, stale := capture(t, clientBundleCheck)
		if stale {
			t.Error("a non-checkout cannot be 'behind'; it is a different problem")
		}
		if !strings.Contains(out, "not a git checkout") {
			t.Errorf("want the non-checkout note, got %q", out)
		}
	})

	t.Run("checkout with no upstream is reported stale", func(t *testing.T) {
		dir := t.TempDir()
		gitInit(t, dir)
		t.Setenv("FLEET_CLIENT_CONFIG_DIR", dir)
		out, stale := capture(t, clientBundleCheck)
		if !stale {
			t.Error("no upstream means fleet update will never advance it — that is the silent-stale case")
		}
		if !strings.Contains(out, "no upstream tracking branch") {
			t.Errorf("want the no-upstream diagnosis, got %q", out)
		}
	})

	t.Run("bundle dir comes from the bootstrap state file", func(t *testing.T) {
		dir := t.TempDir()
		gitInit(t, dir)
		state := t.TempDir()
		if err := os.WriteFile(filepath.Join(state, "client-config.dir"), []byte(dir+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("FLEET_CLIENT_CONFIG_DIR", "")
		t.Setenv("FLEET_STATE_DIR", state)
		out, _ := capture(t, clientBundleCheck)
		if !strings.Contains(out, dir) {
			t.Errorf("state-file fallback should resolve the bundle dir, got %q", out)
		}
	})
}
