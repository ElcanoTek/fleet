package clientconfig

import (
	_ "embed"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/goccy/go-yaml"
)

// Fleet ships a curated directory of HOSTED third-party MCP servers baked into
// the binary (builtin_remote_catalog.yaml) so every deployment gets a rich,
// searchable connector directory without each client bundle copying hundreds of
// lines of listings. Inheritance is on by default because the directory is a
// LISTING ONLY — fleet never contacts an entry's URL until a user explicitly
// adds it through the per-user remote-MCP OAuth flow (#443). That listing-only
// property is load-bearing: any change that makes catalog entries auto-connect
// must revisit the default here (see docs/MCP-CATALOG.md).
//
// Merge contract (mergeBuiltinRemoteCatalog):
//   - bundle remote_mcp_catalog entries always win over a built-in entry with
//     the same name (curated override);
//   - remote_mcp_catalog_hidden tombstones drop individual built-in entries;
//   - community-provenance built-in entries are inherited only when the bundle
//     opts in via remote_mcp_catalog_community (the silently-inherited surface
//     stays vendor-official + labeled aggregators);
//   - a built-in entry whose name collides with a bundled mcp_servers connector
//     is skipped with a LOUD log line, never silently (the bundle author must
//     be able to see why a listing vanished). Bundle-authored entries keep the
//     hard validate error for the same collision.

//go:embed builtin_remote_catalog.yaml
var builtinRemoteCatalogYAML []byte

type builtinCatalogFile struct {
	Servers []RemoteMCPCatalogEntry `yaml:"servers"`
}

var (
	builtinCatalogOnce sync.Once
	builtinCatalog     []RemoteMCPCatalogEntry
	errBuiltinCatalog  error
)

// loadBuiltinRemoteCatalog parses and fully validates the embedded directory
// exactly once. Unlike bundle entries, built-in entries must carry explicit
// provenance, a category, and a docs_url — trust and grouping are never
// inherited by omission in the file every deployment receives. A unit test
// runs this same loader so a malformed entry fails CI, not a customer boot.
func loadBuiltinRemoteCatalog() ([]RemoteMCPCatalogEntry, error) {
	builtinCatalogOnce.Do(func() {
		var f builtinCatalogFile
		if err := yaml.UnmarshalWithOptions(builtinRemoteCatalogYAML, &f, yaml.Strict()); err != nil {
			errBuiltinCatalog = fmt.Errorf("parse builtin remote MCP catalog: %w", err)
			return
		}
		seen := map[string]bool{}
		for i := range f.Servers {
			e := &f.Servers[i]
			name := strings.TrimSpace(e.Name)
			switch {
			case name == "":
				errBuiltinCatalog = fmt.Errorf("builtin remote MCP catalog[%d]: name is required", i)
			case seen[name]:
				errBuiltinCatalog = fmt.Errorf("builtin remote MCP catalog: duplicate name %q", name)
			case strings.TrimSpace(e.DisplayName) == "":
				errBuiltinCatalog = fmt.Errorf("builtin remote MCP catalog[%q]: display_name is required", name)
			case strings.TrimSpace(e.Description) == "":
				errBuiltinCatalog = fmt.Errorf("builtin remote MCP catalog[%q]: description is required", name)
			case !strings.HasPrefix(strings.TrimSpace(e.URL), "https://"):
				errBuiltinCatalog = fmt.Errorf("builtin remote MCP catalog[%q]: url must be https:// (got %q)", name, e.URL)
			case strings.TrimSpace(e.Vendor) == "":
				errBuiltinCatalog = fmt.Errorf("builtin remote MCP catalog[%q]: vendor is required for trust labeling", name)
			case strings.TrimSpace(e.DocsURL) == "":
				errBuiltinCatalog = fmt.Errorf("builtin remote MCP catalog[%q]: docs_url is required so users can vet the server", name)
			case e.Provenance == "":
				errBuiltinCatalog = fmt.Errorf("builtin remote MCP catalog[%q]: provenance is required (official|third_party|community)", name)
			case e.Category == "":
				errBuiltinCatalog = fmt.Errorf("builtin remote MCP catalog[%q]: category is required", name)
			case e.Provenance != "official" && strings.TrimSpace(e.RepoURL) == "" && strings.TrimSpace(e.Vendor) == "":
				// Unreachable given the vendor check above; kept as the written
				// rule: a non-official entry must be attributable.
				errBuiltinCatalog = fmt.Errorf("builtin remote MCP catalog[%q]: non-official entry must be attributable", name)
			}
			if errBuiltinCatalog != nil {
				return
			}
			seen[name] = true
			if err := validateRemoteMCPEntryMeta(e); err != nil {
				errBuiltinCatalog = fmt.Errorf("builtin remote MCP catalog: %w", err)
				return
			}
			e.Builtin = true
		}
		builtinCatalog = f.Servers
	})
	return builtinCatalog, errBuiltinCatalog
}

// mergeBuiltinRemoteCatalog resolves the embedded directory and delegates to
// mergeRemoteCatalog. builtinKnob is the manifest's remote_mcp_catalog_builtin
// pointer — nil means absent, which defaults to inherit.
func mergeBuiltinRemoteCatalog(bundle []RemoteMCPCatalogEntry, mcpServers []ServerDef, builtinKnob *bool, community bool, hidden []string) ([]RemoteMCPCatalogEntry, error) {
	if builtinKnob != nil && !*builtinKnob {
		return bundle, nil
	}
	builtin, err := loadBuiltinRemoteCatalog()
	if err != nil {
		return nil, err
	}
	return mergeRemoteCatalog(bundle, builtin, mcpServers, community, hidden), nil
}

// mergeRemoteCatalog appends the inherited built-in directory after the
// bundle's own entries (bundle first: a curator's picks lead the listing),
// applying the tombstone/override/community/shadow rules documented above.
func mergeRemoteCatalog(bundle, builtin []RemoteMCPCatalogEntry, mcpServers []ServerDef, community bool, hidden []string) []RemoteMCPCatalogEntry {
	hiddenSet := map[string]bool{}
	for _, h := range hidden {
		hiddenSet[strings.TrimSpace(h)] = true
	}
	overridden := map[string]bool{}
	for _, e := range bundle {
		overridden[strings.TrimSpace(e.Name)] = true
	}
	sandboxed := map[string]bool{}
	for _, s := range mcpServers {
		sandboxed[s.Name] = true
	}
	known := map[string]bool{}
	merged := append([]RemoteMCPCatalogEntry{}, bundle...)
	for _, e := range builtin {
		known[e.Name] = true
		switch {
		case hiddenSet[e.Name], overridden[e.Name]:
		case e.Provenance == "community" && !community:
		case sandboxed[e.Name]:
			// Loud, never silent: the author must be able to see why a
			// built-in listing vanished from the directory.
			log.Printf("clientconfig: warning: builtin remote MCP catalog entry %q is shadowed by the bundle's mcp_servers connector of the same name and was dropped from the directory", e.Name)
		default:
			merged = append(merged, e)
		}
	}
	for h := range hiddenSet {
		if !known[h] {
			log.Printf("clientconfig: warning: remote_mcp_catalog_hidden names %q, which is not a builtin catalog entry (typo?)", h)
		}
	}
	return merged
}
