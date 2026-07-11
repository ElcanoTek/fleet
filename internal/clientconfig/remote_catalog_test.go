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

	t.Run("bundle entries lead and override builtin by name", func(t *testing.T) {
		dir := writeManifest(t, `
remote_mcp_catalog:
  - name: github
    display_name: GitHub (curated)
    description: GitHub's hosted MCP server, bundle-curated copy.
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
		if len(b.RemoteMCPCatalog) < 3 {
			t.Fatalf("want bundle entries + inherited builtin directory, got %d entries", len(b.RemoteMCPCatalog))
		}
		gh := b.RemoteMCPCatalog[0]
		if gh.Name != "github" || gh.DisplayName != "GitHub (curated)" || gh.Builtin {
			t.Errorf("bundle github entry should lead and win over builtin: %+v", gh)
		}
		if b.RemoteMCPCatalog[1].Name != "notion" || b.RemoteMCPCatalog[1].Builtin {
			t.Errorf("bundle order not preserved: %+v", b.RemoteMCPCatalog[1])
		}
		counts := map[string]int{}
		for _, e := range b.RemoteMCPCatalog {
			counts[e.Name]++
			if e.Name != "github" && e.Name != "notion" && !e.Builtin {
				t.Errorf("entry %q should be marked builtin", e.Name)
			}
		}
		if counts["github"] != 1 || counts["notion"] != 1 {
			t.Errorf("override must dedupe by name: github=%d notion=%d", counts["github"], counts["notion"])
		}
	})

	t.Run("absent section inherits the builtin directory", func(t *testing.T) {
		dir := writeManifest(t, "mcp_servers: []\n")
		b, err := Load(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(b.RemoteMCPCatalog) == 0 {
			t.Fatal("want inherited builtin directory, got empty catalog")
		}
		for _, e := range b.RemoteMCPCatalog {
			if !e.Builtin {
				t.Errorf("entry %q should be marked builtin", e.Name)
			}
			if e.Provenance == "community" {
				t.Errorf("community entry %q inherited without remote_mcp_catalog_community opt-in", e.Name)
			}
		}
	})

	t.Run("builtin false opts out entirely", func(t *testing.T) {
		dir := writeManifest(t, "remote_mcp_catalog_builtin: false\n")
		b, err := Load(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(b.RemoteMCPCatalog) != 0 {
			t.Errorf("want empty catalog with builtin opted out, got %d entries", len(b.RemoteMCPCatalog))
		}
	})

	t.Run("hidden tombstones drop individual builtin entries", func(t *testing.T) {
		dir := writeManifest(t, "remote_mcp_catalog_hidden: [github, notion]\n")
		b, err := Load(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(b.RemoteMCPCatalog) == 0 {
			t.Fatal("hiding two entries must not empty the directory")
		}
		for _, e := range b.RemoteMCPCatalog {
			if e.Name == "github" || e.Name == "notion" {
				t.Errorf("hidden entry %q still present", e.Name)
			}
		}
	})

	t.Run("bundled mcp_servers name shadows builtin entry without failing the load", func(t *testing.T) {
		dir := writeManifest(t, `
mcp_servers:
  - name: github
    type: http
    url: "https://internal.test/mcp"
    always: true
`)
		b, err := Load(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		for _, e := range b.RemoteMCPCatalog {
			if e.Name == "github" {
				t.Errorf("builtin github should be shadowed by the sandboxed connector of the same name")
			}
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
		{
			"unknown provenance",
			`
remote_mcp_catalog:
  - name: x
    display_name: X
    description: d
    url: "https://x.test/mcp"
    provenance: vendor
`,
			"unknown provenance",
		},
		{
			"unknown auth",
			`
remote_mcp_catalog:
  - name: x
    display_name: X
    description: d
    url: "https://x.test/mcp"
    auth: password
`,
			"unknown auth",
		},
		{
			"malformed category",
			`
remote_mcp_catalog:
  - name: x
    display_name: X
    description: d
    url: "https://x.test/mcp"
    category: "CRM Sales"
`,
			"lowercase kebab-case",
		},
		{
			"plain-http repo_url",
			`
remote_mcp_catalog:
  - name: x
    display_name: X
    description: d
    url: "https://x.test/mcp"
    repo_url: "http://github.com/x/x"
`,
			"repo_url must be https",
		},
		{
			"uppercase tag",
			`
remote_mcp_catalog:
  - name: x
    display_name: X
    description: d
    url: "https://x.test/mcp"
    tags: [CRM]
`,
			"must be lowercase",
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

// TestBuiltinRemoteCatalog keeps the embedded directory honest: it must parse,
// be substantial (the whole point is a rich out-of-the-box directory), and
// every entry must carry EXPLICIT provenance, a category, tags, and a docs_url
// — trust and grouping are never inherited by omission in the file every
// deployment silently receives (the loader enforces this; the test makes a
// malformed entry fail CI instead of a customer boot).
func TestBuiltinRemoteCatalog(t *testing.T) {
	entries, err := loadBuiltinRemoteCatalog()
	if err != nil {
		t.Fatalf("builtin catalog: %v", err)
	}
	if len(entries) < 60 {
		t.Fatalf("builtin catalog should be substantial, got only %d entries", len(entries))
	}
	categories := map[string]int{}
	for _, e := range entries {
		categories[e.Category]++
		if !e.Builtin {
			t.Errorf("entry %q: loader must mark entries builtin", e.Name)
		}
		if len(e.Tags) == 0 {
			t.Errorf("entry %q: builtin entries must be tagged for search", e.Name)
		}
		if e.Auth == "" {
			t.Errorf("entry %q: builtin entries must carry an auth hint", e.Name)
		}
		if strings.Contains(e.URL, "{") != (e.Auth == "tenant") {
			t.Errorf("entry %q: a {placeholder} URL and auth=tenant must imply each other (url=%q auth=%q)", e.Name, e.URL, e.Auth)
		}
		if e.Provenance == "community" && strings.TrimSpace(e.RepoURL) == "" {
			t.Errorf("entry %q: a community-hosted entry must link its source repo so users can vet it", e.Name)
		}
	}
	if len(categories) < 8 {
		t.Errorf("builtin catalog should span many categories, got %d: %v", len(categories), categories)
	}

	// X documents its API-capable XMCP as self-hosted; api.x.com/mcp is not a
	// hosted MCP endpoint. Keep only X's documented hosted Docs MCP so the
	// connection UI never sends users into OAuth discovery for a nonexistent
	// service.
	byName := make(map[string]RemoteMCPCatalogEntry, len(entries))
	for _, e := range entries {
		byName[e.Name] = e
	}
	if _, ok := byName["x"]; ok {
		t.Error("builtin catalog must not advertise the nonexistent hosted X API MCP")
	}
	xDocs, ok := byName["x-docs"]
	if !ok {
		t.Fatal("builtin catalog should include X's hosted Docs MCP")
	}
	if xDocs.URL != "https://docs.x.com/mcp" || xDocs.Auth != "open" {
		t.Errorf("x-docs = %+v, want the documented open hosted endpoint", xDocs)
	}
}

// TestMergeRemoteCatalog exercises the inheritance rules against a fabricated
// builtin list so community gating and shadow behavior are covered without
// depending on the shipped catalog's contents.
func TestMergeRemoteCatalog(t *testing.T) {
	builtin := []RemoteMCPCatalogEntry{
		{Name: "alpha", Provenance: "official", Builtin: true},
		{Name: "beta", Provenance: "community", Builtin: true},
		{Name: "gamma", Provenance: "third_party", Builtin: true},
		{Name: "shadowed", Provenance: "official", Builtin: true},
	}
	servers := []ServerDef{{Name: "shadowed"}}
	bundle := []RemoteMCPCatalogEntry{{Name: "alpha", DisplayName: "Alpha (curated)"}}

	names := func(entries []RemoteMCPCatalogEntry) []string {
		out := make([]string, 0, len(entries))
		for _, e := range entries {
			out = append(out, e.Name)
		}
		return out
	}

	// Without the community opt-in and with gamma hidden, everything except the
	// bundle's own alpha drops: builtin alpha is overridden, beta is community,
	// gamma is tombstoned, shadowed collides with an mcp_servers connector.
	got := mergeRemoteCatalog(bundle, builtin, servers, false, []string{"gamma"})
	if len(got) != 1 || got[0].Name != "alpha" || got[0].Builtin || got[0].DisplayName != "Alpha (curated)" {
		t.Fatalf("merge without community: got %v", names(got))
	}

	got = mergeRemoteCatalog(bundle, builtin, servers, true, nil)
	// bundle alpha + builtin beta (community opted in) + gamma; shadowed dropped.
	if len(got) != 3 || got[0].Name != "alpha" || got[1].Name != "beta" || got[2].Name != "gamma" {
		t.Fatalf("merge with community: got %v", names(got))
	}
}
