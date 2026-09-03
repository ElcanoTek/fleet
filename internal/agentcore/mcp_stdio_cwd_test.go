package agentcore

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStdioCwd covers the decision that keeps MCP subprocesses from writing
// into the operator's client-bundle git checkout.
func TestStdioCwd(t *testing.T) {
	ws := t.TempDir()
	bundle := t.TempDir()

	t.Run("workspace wins over the bundle root", func(t *testing.T) {
		if got := StdioCwd(bundle, false, ws); got != ws {
			t.Errorf("StdioCwd = %q, want the workspace %q", got, ws)
		}
	})

	t.Run("pinned dir is never displaced", func(t *testing.T) {
		// An Agent Plugin's root is a spec contract (ADR-0054): its args are
		// opaque and it resolves bundled files against that root.
		if got := StdioCwd(bundle, true, ws); got != bundle {
			t.Errorf("StdioCwd = %q, want the pinned dir %q", got, bundle)
		}
	})

	t.Run("no workspace falls back", func(t *testing.T) {
		for _, w := range []string{"", "   "} {
			if got := StdioCwd(bundle, false, w); got != bundle {
				t.Errorf("StdioCwd(%q) = %q, want the fallback %q", w, got, bundle)
			}
		}
	})

	// The regression that matters: exec refuses to start a process whose cwd is
	// missing, so pointing at an un-materialized workspace would stop servers
	// booting entirely. A scope can carry a path nothing has created yet.
	t.Run("a missing workspace falls back rather than breaking the spawn", func(t *testing.T) {
		missing := filepath.Join(ws, "not-created-yet")
		if got := StdioCwd(bundle, false, missing); got != bundle {
			t.Errorf("StdioCwd = %q, want the fallback %q for a non-existent workspace", got, bundle)
		}
	})

	t.Run("a file is not a working directory", func(t *testing.T) {
		f := filepath.Join(ws, "afile")
		if err := writeFileForTest(f); err != nil {
			t.Fatal(err)
		}
		if got := StdioCwd(bundle, false, f); got != bundle {
			t.Errorf("StdioCwd = %q, want the fallback %q for a regular file", got, bundle)
		}
	})
}

func writeFileForTest(path string) error {
	return os.WriteFile(path, []byte("x"), 0o600)
}
