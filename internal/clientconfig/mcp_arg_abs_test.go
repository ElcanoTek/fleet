package clientconfig

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAbsolutizeScriptArgs — bundle-relative script args become absolute at
// load, which is what frees the subprocess cwd from having to be the bundle
// root. Until then, every server's relative output path resolved into the
// operator's git checkout.
func TestAbsolutizeScriptArgs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "mcp"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "mcp", "server.py")
	if err := os.WriteFile(script, []byte("#\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("a resolving script arg is absolutized", func(t *testing.T) {
		got := absolutizeScriptArgs(dir, []string{"mcp/server.py"})
		if len(got) != 1 || got[0] != script {
			t.Errorf("got %v, want [%s]", got, script)
		}
	})

	t.Run("flags and non-script args are untouched", func(t *testing.T) {
		in := []string{"-u", "--flag=mcp/server.py.bak", "mcp/server.py"}
		got := absolutizeScriptArgs(dir, in)
		if got[0] != "-u" || got[1] != "--flag=mcp/server.py.bak" || got[2] != script {
			t.Errorf("got %v", got)
		}
	})

	t.Run("an absolute arg is left alone", func(t *testing.T) {
		got := absolutizeScriptArgs(dir, []string{script})
		if got[0] != script {
			t.Errorf("got %v", got)
		}
	})

	// A typo must stay recognisable: ValidateMCPArgPaths reports it against the
	// spelling the author wrote, so rewriting it here would only make the
	// warning harder to match to the manifest.
	t.Run("a non-resolving script arg is left as written", func(t *testing.T) {
		got := absolutizeScriptArgs(dir, []string{"mcp/typo.py"})
		if got[0] != "mcp/typo.py" {
			t.Errorf("got %v, want the original spelling", got)
		}
	})

	t.Run("a directory is not a script", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(dir, "d.py"), 0o755); err != nil {
			t.Fatal(err)
		}
		got := absolutizeScriptArgs(dir, []string{"d.py"})
		if got[0] != "d.py" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("no args and no bundle are no-ops", func(t *testing.T) {
		if got := absolutizeScriptArgs(dir, nil); got != nil {
			t.Errorf("got %v", got)
		}
		if got := absolutizeScriptArgs("", []string{"mcp/server.py"}); got[0] != "mcp/server.py" {
			t.Errorf("got %v", got)
		}
	})
}

// TestBundleAbsolutizesArgsAndMarksPin — the load-level contract: a bundle
// server's args come out absolute (so cwd is free) with Dir as an UNPINNED
// fallback, while an Agent Plugin keeps both its opaque args and its pinned
// plugin root.
func TestBundleAbsolutizesArgsAndMarksPin(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "mcp"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "mcp", "srv.py")
	if err := os.WriteFile(script, []byte("#\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := `
mcp_servers:
  - name: srv
    always: true
    type: stdio
    command: python3
    args: ["mcp/srv.py"]
    display_name: "Server"
    description: "Do a thing. No credentials required."
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	sc := b.MCPServerConfigs()["srv"]
	if len(sc.Args) != 1 || sc.Args[0] != script {
		t.Errorf("args = %v, want the absolute script path [%s]", sc.Args, script)
	}
	if sc.DirPinned {
		t.Error("a plain bundle server must NOT pin its cwd — only Agent Plugins do")
	}
	if sc.Dir == "" {
		t.Error("Dir should still carry the bundle root as the fallback cwd")
	}
}
