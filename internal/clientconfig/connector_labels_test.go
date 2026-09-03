package clientconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeriveDisplayName(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"openx_mcp", "Openx"},
		{"indexexchange_mcp", "Indexexchange"},
		{"mcp_gamma", "Gamma"},
		{"knowledge_base", "Knowledge Base"},
		{"s3_feeds", "S3 Feeds"},
		{"omnicom_s3_feeds", "Omnicom S3 Feeds"},
		{"email", "Email"},
		{"fast-io", "Fast Io"},
		// Author casing survives — the helper only touches the first rune.
		{"openX", "OpenX"},
		// All-noise and empty names fall back to the raw name rather than
		// rendering a blank label in the picker.
		{"mcp", "mcp"},
		{"mcp_server", "mcp_server"},
		{"", ""},
		{"  spaced_name  ", "Spaced Name"},
	}
	for _, tc := range cases {
		if got := deriveDisplayName(tc.name); got != tc.want {
			t.Errorf("deriveDisplayName(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestConnectorCopyFallback pins the two guarantees the Tools picker depends
// on: a connector that omits display_name still renders a human-shaped label
// (never a raw wire identifier), and a connector that declares its own copy is
// never overwritten by the derivation.
func TestConnectorCopyFallback(t *testing.T) {
	dir := t.TempDir()
	body := `
mcp_servers:
  - name: openx_mcp
    optional: true
    always: true
    type: stdio
    command: python3
    args: ["mcp/openx_mcp.py"]
  - name: knowledge_base
    always: true
    type: stdio
    command: python3
    args: ["mcp/knowledge_base.py"]
    display_name: "Team Knowledge Base"
    description: "Search and read the bundled company handbook."
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfgs := b.MCPServerConfigs()

	if got := cfgs["openx_mcp"].DisplayName; got != "Openx" {
		t.Errorf("missing display_name should fall back to the derived label, got %q", got)
	}
	if got := cfgs["openx_mcp"].Description; got != "" {
		t.Errorf("description is never invented, got %q", got)
	}

	if got := cfgs["knowledge_base"].DisplayName; got != "Team Knowledge Base" {
		t.Errorf("declared display_name must win, got %q", got)
	}
	if got := cfgs["knowledge_base"].Description; got != "Search and read the bundled company handbook." {
		t.Errorf("declared description must survive, got %q", got)
	}

	// Always-on rows read the same fields, so the fallback has to reach them
	// too — an always-on connector is the one a user cannot switch off and
	// most needs explained.
	always := b.AlwaysOnServers()
	if len(always) != 1 || always[0].Name != "knowledge_base" || always[0].DisplayName != "Team Knowledge Base" {
		t.Errorf("always-on catalog wrong: %+v", always)
	}
}
