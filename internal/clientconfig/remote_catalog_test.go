package clientconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRemoteMCPCatalog covers the third-party hosted MCP directory (#538): a
// well-formed section threads through to Bundle.RemoteMCPCatalog in manifest
// order, and malformed entries fail the load loudly (fail-loud-at-startup,
// like every other manifest section).
func TestRemoteMCPCatalog(t *testing.T) {
	writeManifest := func(t *testing.T, body string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("valid catalog threads through in order", func(t *testing.T) {
		dir := writeManifest(t, `
remote_mcp_catalog:
  - name: github
    display_name: GitHub
    description: GitHub's hosted MCP server.
    url: "https://api.githubcopilot.com/mcp/"
    vendor: GitHub, Inc.
    docs_url: "https://docs.github.com/mcp"
  - name: notion
    display_name: Notion
    description: Notion's hosted MCP server.
    url: "https://mcp.notion.com/mcp"
`)
		b, err := Load(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(b.RemoteMCPCatalog) != 2 {
			t.Fatalf("want 2 entries, got %d", len(b.RemoteMCPCatalog))
		}
		gh := b.RemoteMCPCatalog[0]
		if gh.Name != "github" || gh.DisplayName != "GitHub" || gh.Vendor != "GitHub, Inc." ||
			gh.URL != "https://api.githubcopilot.com/mcp/" || gh.DocsURL != "https://docs.github.com/mcp" {
			t.Errorf("github entry wrong: %+v", gh)
		}
		if b.RemoteMCPCatalog[1].Name != "notion" {
			t.Errorf("order not preserved: %+v", b.RemoteMCPCatalog[1])
		}
	})

	t.Run("absent section is an empty catalog", func(t *testing.T) {
		dir := writeManifest(t, "mcp_servers: []\n")
		b, err := Load(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(b.RemoteMCPCatalog) != 0 {
			t.Errorf("want empty catalog, got %+v", b.RemoteMCPCatalog)
		}
	})

	rejects := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			"missing name",
			`
remote_mcp_catalog:
  - display_name: X
    description: d
    url: "https://x.test/mcp"
`,
			"name is required",
		},
		{
			"duplicate name",
			`
remote_mcp_catalog:
  - name: x
    display_name: X
    description: d
    url: "https://x.test/mcp"
  - name: x
    display_name: X2
    description: d2
    url: "https://x2.test/mcp"
`,
			"duplicate name",
		},
		{
			"missing display_name",
			`
remote_mcp_catalog:
  - name: x
    description: d
    url: "https://x.test/mcp"
`,
			"display_name is required",
		},
		{
			"missing description",
			`
remote_mcp_catalog:
  - name: x
    display_name: X
    url: "https://x.test/mcp"
`,
			"description is required",
		},
		{
			"missing url",
			`
remote_mcp_catalog:
  - name: x
    display_name: X
    description: d
`,
			"url is required",
		},
		{
			"plain-http url",
			`
remote_mcp_catalog:
  - name: x
    display_name: X
    description: d
    url: "http://x.test/mcp"
`,
			"must be https",
		},
		{
			"collides with bundled server",
			`
mcp_servers:
  - name: github
    type: http
    url: "https://internal.test/mcp"
    always: true
remote_mcp_catalog:
  - name: github
    display_name: GitHub
    description: d
    url: "https://api.githubcopilot.com/mcp/"
`,
			"collides with bundled",
		},
	}
	for _, tc := range rejects {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			dir := writeManifest(t, tc.body)
			_, err := Load(dir)
			if err == nil {
				t.Fatal("load should fail")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestDefaultBundleRemoteMCPCatalogValid asserts the shipped generic bundle's
// curated third-party directory loads clean and every entry is https with docs
// — the same keep-the-shipped-bundle-honest pattern as the skills/evals tests.
func TestDefaultBundleRemoteMCPCatalogValid(t *testing.T) {
	b, err := Load(filepath.Join(repoRoot(t), "config", "default"))
	if err != nil {
		t.Fatalf("load default bundle: %v", err)
	}
	if len(b.RemoteMCPCatalog) == 0 {
		t.Fatal("generic bundle should ship a non-empty curated third-party catalog")
	}
	for _, e := range b.RemoteMCPCatalog {
		if !strings.HasPrefix(e.URL, "https://") {
			t.Errorf("entry %q: url %q not https", e.Name, e.URL)
		}
		if strings.TrimSpace(e.Vendor) == "" {
			t.Errorf("entry %q: vendor should be named for trust labeling", e.Name)
		}
		if strings.TrimSpace(e.DocsURL) == "" {
			t.Errorf("entry %q: docs_url should point at vendor documentation", e.Name)
		}
	}
}
