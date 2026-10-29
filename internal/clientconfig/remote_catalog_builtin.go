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
		manualStems := map[string]string{}
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
			case (e.Auth == "tenant" || e.Auth == "api_key") && strings.TrimSpace(e.SetupHint) == "":
				// An entry a user cannot one-click add must SAY how to get
				// connected — a "needs your URL"/"API key" badge with no
				// instructions is advertising, not onboarding.
				errBuiltinCatalog = fmt.Errorf("builtin remote MCP catalog[%q]: auth %q requires a setup_hint telling the user where the URL/key comes from", name, e.Auth)
			}
			if errBuiltinCatalog != nil {
				return
			}
			seen[name] = true
			if err := validateRemoteMCPEntryMeta(e); err != nil {
				errBuiltinCatalog = fmt.Errorf("builtin remote MCP catalog: %w", err)
				return
			}
			if e.ClientRegistration == "manual" {
				stem := manualOAuthStem(name)
				if stem == "" {
					errBuiltinCatalog = fmt.Errorf("builtin remote MCP catalog[%q]: client_registration: manual requires an alphanumeric name stem", name)
					return
				}
				if prev, dup := manualStems[stem]; dup {
					errBuiltinCatalog = fmt.Errorf("builtin remote MCP catalog[%q]: manual OAuth env stem %q collides with %q", name, stem, prev)
					return
				}
				manualStems[stem] = name
			}
			e.Builtin = true
		}
		builtinCatalog = f.Servers
	})
	return builtinCatalog, errBuiltinCatalog
}

// BuiltinRemoteCatalog returns the embedded directory as shipped — every
// entry, before any bundle's overrides, hidden list or community opt-in
// narrow it. That is the view a check of the DIRECTORY wants (the nightly
// catalog smoke in internal/remotemcp, which must not skip an entry just
// because the default bundle leaves community entries off); a deployment's
// view is Bundle.RemoteMCPCatalog. The slice is a copy.
func BuiltinRemoteCatalog() ([]RemoteMCPCatalogEntry, error) {
	entries, err := loadBuiltinRemoteCatalog()
	if err != nil {
		return nil, err
	}
	return append([]RemoteMCPCatalogEntry(nil), entries...), nil
}

// mergeBuiltinRemoteCatalog resolves the embedded directory and delegates to
// mergeRemoteCatalog. builtinKnob is the manifest's remote_mcp_catalog_builtin
// pointer — nil means absent, which defaults to inherit.
func mergeBuiltinRemoteCatalog(bundle []RemoteMCPCatalogEntry, mcpServers []ServerDef, builtinKnob *bool, community bool, hidden []string) ([]RemoteMCPCatalogEntry, error) {
	if builtinKnob != nil && !*builtinKnob {
		resolveCatalogOAuthClients(bundle)
		return bundle, nil
	}
	builtin, err := loadBuiltinRemoteCatalog()
	if err != nil {
		return nil, err
	}
	merged := mergeRemoteCatalog(bundle, builtin, mcpServers, community, hidden)
	resolveCatalogOAuthClients(merged)
	return merged, nil
}

// manualOAuthStem normalizes a catalog entry name into an uppercase
// alphanumeric/underscore stem for environment-variable lookup
// ("slack" -> "SLACK", "google-drive" -> "GOOGLE_DRIVE").
func manualOAuthStem(entryName string) string {
	name := strings.TrimSpace(entryName)
	if name == "" {
		return ""
	}
	var sb strings.Builder
	sb.Grow(len(name))
	for _, r := range strings.ToUpper(name) {
		switch {
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	return strings.Trim(sb.String(), "_")
}

// ManualOAuthEnvVarNames returns the candidate process-env variable names an
// operator may set to supply a shared OAuth client ID and secret for a
// client_registration: manual directory entry (in precedence order):
//
//	ID:     FLEET_OAUTH_CLIENT_ID_<STEM>, <STEM>_MCP_CLIENT_ID
//	Secret: FLEET_OAUTH_CLIENT_SECRET_<STEM>, <STEM>_MCP_CLIENT_SECRET
//
// where <STEM> is the entry name uppercased with hyphens turned into
// underscores (e.g. "slack" -> SLACK_MCP_CLIENT_ID / SLACK_MCP_CLIENT_SECRET,
// "google-drive" -> GOOGLE_DRIVE_MCP_CLIENT_ID / GOOGLE_DRIVE_MCP_CLIENT_SECRET).
func ManualOAuthEnvVarNames(entryName string) (idVars, secretVars []string) {
	stem := manualOAuthStem(entryName)
	if stem == "" {
		return nil, nil
	}
	return []string{"FLEET_OAUTH_CLIENT_ID_" + stem, stem + "_MCP_CLIENT_ID"},
		[]string{"FLEET_OAUTH_CLIENT_SECRET_" + stem, stem + "_MCP_CLIENT_SECRET"}
}

func hasManualOAuthIDInEnv(entryName string) bool {
	idVars, _ := ManualOAuthEnvVarNames(entryName)
	for _, k := range idVars {
		if _, found := lookupNonEmpty(k); found {
			return true
		}
	}
	return false
}

func hasManualOAuthSecretInEnv(entryName string) bool {
	_, secVars := ManualOAuthEnvVarNames(entryName)
	for _, k := range secVars {
		if _, found := lookupNonEmpty(k); found {
			return true
		}
	}
	return false
}

// EffectiveOAuthClient resolves the operator-provisioned OAuth client for a
// client_registration: manual directory entry, checking explicit manifest/boot
// fields (OAuthClientID / OAuthClientSecret) first and then, when the entry has
// not yet been boot-snapshotted by resolveCatalogOAuthClients, the conventional
// environment variables (ManualOAuthEnvVarNames). For an entry with
// client_secret: required, both client ID and secret must be non-empty for ok
// to be true, so a half-configured environment never hides the manual form
// only to fail at add time with ErrClientSecretRequired.
func (e RemoteMCPCatalogEntry) EffectiveOAuthClient() (clientID, clientSecret string, ok bool) {
	if e.ClientRegistration != "manual" {
		return "", "", false
	}
	if e.Auth != "" && e.Auth != "oauth" && e.Auth != "tenant" {
		return "", "", false
	}
	clientID = strings.TrimSpace(e.OAuthClientID)
	clientSecret = strings.TrimSpace(e.OAuthClientSecret)
	if !e.oauthEnvResolved && (clientID == "" || clientSecret == "") {
		idVars, secVars := ManualOAuthEnvVarNames(e.Name)
		if clientID == "" {
			for _, k := range idVars {
				if v, found := lookupNonEmpty(k); found {
					clientID = v
					break
				}
			}
		}
		if clientSecret == "" {
			for _, k := range secVars {
				if v, found := lookupNonEmpty(k); found {
					clientSecret = v
					break
				}
			}
		}
	}
	if clientID == "" {
		return "", "", false
	}
	if strings.TrimSpace(e.ClientSecret) == "required" && clientSecret == "" {
		return "", "", false
	}
	return clientID, clientSecret, true
}

// resolveCatalogOAuthClients snapshots any operator-provisioned OAuth client
// credentials from the process environment onto the per-Bundle catalog slice
// at Load time (after applyBootEnvFile) so runtime HTTP handlers read the
// resolved fields without touching os.Getenv concurrently with config.Reload.
func resolveCatalogOAuthClients(entries []RemoteMCPCatalogEntry) {
	for i := range entries {
		cid, csec, ok := entries[i].EffectiveOAuthClient()
		if ok {
			entries[i].OAuthClientID = cid
			entries[i].OAuthClientSecret = csec
		} else {
			entries[i].OAuthClientID = ""
			entries[i].OAuthClientSecret = ""
		}
		entries[i].oauthEnvResolved = true
	}
}

// manualOAuthEnvNamesFor returns the <STEM>_MCP_CLIENT_ID and
// <STEM>_MCP_CLIENT_SECRET env-var names for every manual-registration entry
// in the built-in catalog plus any bundle-declared remote_mcp_catalog entries,
// so Load can register them with config.RegisterAllowedEnvVars before reading
// FLEET_ENV_FILE.
func manualOAuthEnvNamesFor(bundleEntries []RemoteMCPCatalogEntry) []string {
	var out []string
	addEntry := func(e RemoteMCPCatalogEntry) {
		if e.ClientRegistration != "manual" && strings.TrimSpace(e.Name) == "" {
			return
		}
		ids, secs := ManualOAuthEnvVarNames(e.Name)
		out = append(out, ids...)
		out = append(out, secs...)
	}
	for _, e := range bundleEntries {
		addEntry(e)
	}
	if builtin, err := loadBuiltinRemoteCatalog(); err == nil {
		for _, e := range builtin {
			if e.ClientRegistration == "manual" {
				addEntry(e)
			}
		}
	}
	return out
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
	overriddenStems := map[string]bool{}
	for _, e := range bundle {
		name := strings.TrimSpace(e.Name)
		overridden[name] = true
		if stem := manualOAuthStem(name); stem != "" {
			overriddenStems[stem] = true
		}
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
		case e.ClientRegistration == "manual" && overriddenStems[manualOAuthStem(e.Name)]:
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
