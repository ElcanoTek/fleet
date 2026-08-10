package clientconfig_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	clientconfig "github.com/ElcanoTek/fleet/internal/clientconfig"
)

var spriteSymbolRe = regexp.MustCompile(`<symbol\s+id="([^"]+)"`)

// spriteSymbolIDs reads the symbol ids the web's icon sprite defines. The sprite
// is the single source of truth for which icon names a bundle may name — the
// in-repo counterpart to this check is web/src/app/spriteCoverage.test.ts, which
// covers fleet's own references and the built-in config/default bundle.
func spriteSymbolIDs(t *testing.T) map[string]bool {
	t.Helper()
	const sprite = "../../web/public/icons/core-icons.svg"
	svg, err := os.ReadFile(sprite)
	if err != nil {
		t.Fatalf("read icon sprite %s: %v", sprite, err)
	}
	ids := map[string]bool{}
	for _, m := range spriteSymbolRe.FindAllStringSubmatch(string(svg), -1) {
		ids[m[1]] = true
	}
	if len(ids) < 40 {
		// A sprite that parsed to (almost) nothing would make the icon check
		// below fail on every card instead of only the broken ones.
		t.Fatalf("icon sprite parsed to %d symbols; expected the full set", len(ids))
	}
	return ids
}

// TestRealBundleSanity is an opt-in SANITY check against one or more REAL
// out-of-repo client bundles. It is skipped unless FLEET_SANITY_BUNDLE_DIR is
// set (a single bundle dir, or several separated by the OS path-list
// separator), so it never fails for contributors who don't have a private
// bundle checked out. It is not a fixture the generic fleet ships — it
// validates the pluggable contract end to end against a real bundle:
//
//   - the bundle parses (Load) and carries branding;
//   - with every env var the manifest declares set, every gated or always-on
//     MCP catalog entry resolves into MCPServerConfigs with its ${ENV_VAR}
//     references fully interpolated;
//   - the bundle's skills and MCP arg paths validate clean.
//
// Run it locally with e.g.:
//
//	FLEET_SANITY_BUNDLE_DIR=/path/to/your-config go test ./internal/clientconfig -run TestRealBundleSanity
func TestRealBundleSanity(t *testing.T) {
	env := os.Getenv("FLEET_SANITY_BUNDLE_DIR")
	if strings.TrimSpace(env) == "" {
		t.Skip("FLEET_SANITY_BUNDLE_DIR not set; skipping real-bundle sanity check")
	}
	for _, dir := range filepath.SplitList(env) {
		dir := strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		dir = filepath.Clean(dir)
		t.Run(filepath.Base(dir), func(t *testing.T) {
			if _, err := os.Stat(dir); err != nil {
				t.Fatalf("FLEET_SANITY_BUNDLE_DIR entry %q not usable: %v", dir, err)
			}
			b, err := clientconfig.Load(dir)
			if err != nil {
				t.Fatalf("load bundle %q: %v", dir, err)
			}

			// The manifest names every env var its servers/tools/webhooks need.
			// Give each one a placeholder so credential-gated servers switch on
			// and ${VAR} references resolve. t.Setenv restores originals.
			const placeholder = "sanity-placeholder"
			for _, name := range b.EnvVarNames() {
				t.Setenv(name, placeholder)
			}

			for _, tc := range []struct {
				name  string
				check func(t *testing.T)
			}{
				{"branding", func(t *testing.T) {
					if strings.TrimSpace(b.Branding.AppName) == "" {
						t.Error("Branding.AppName is empty")
					}
				}},
				{"mcp catalog gates on with creds", func(t *testing.T) {
					cfgs := b.MCPServerConfigs()
					for i := range b.MCPCatalog {
						s := &b.MCPCatalog[i]
						gated := s.Always || len(s.EnabledEnv) > 0 || len(s.EnabledGroups) > 0
						if !gated {
							continue // no gate declared: default OFF by design
						}
						if _, ok := cfgs[s.Name]; !ok {
							t.Errorf("server %q declared in catalog but absent from MCPServerConfigs with all creds set", s.Name)
						}
					}
				}},
				{"env refs fully interpolated", func(t *testing.T) {
					// ${FLEET_WORKSPACE} / ${FLEET_TASK_ID} are RESERVED
					// spawn-time tokens substituted per-run by the MCP
					// launcher, so they legitimately survive load-time
					// resolution.
					reserved := func(v string) string {
						v = strings.ReplaceAll(v, "${FLEET_WORKSPACE}", "")
						return strings.ReplaceAll(v, "${FLEET_TASK_ID}", "")
					}
					for name, cfg := range b.MCPServerConfigs() {
						for k, v := range cfg.Env {
							if strings.Contains(reserved(v), "${") {
								t.Errorf("server %q env %q = %q: unresolved ${...} reference", name, k, v)
							}
						}
						for k, v := range cfg.Headers {
							if strings.Contains(v, "${") {
								t.Errorf("server %q header %q = %q: unresolved ${...} reference", name, k, v)
							}
						}
					}
				}},
				{"skills validate", func(t *testing.T) {
					for _, problem := range b.ValidateSkills() {
						t.Errorf("skill validation: %s", problem)
					}
				}},
				{"mcp arg paths validate", func(t *testing.T) {
					for _, problem := range b.ValidateMCPArgPaths() {
						t.Errorf("mcp arg path validation: %s", problem)
					}
				}},
				{"empty-state card icons exist in the sprite", func(t *testing.T) {
					// A card's `icon` is a symbol id in the web's core-icons
					// sprite, rendered as <use href="…#id">. A name the sprite
					// lacks fails SILENTLY — no console error, no failed
					// request, just an empty icon box on the chat home screen.
					// That is how a bundle shipped `globe` and `mail` cards
					// that rendered blank while its `search` and `bar-chart`
					// cards looked fine.
					//
					// Go deliberately treats cards as opaque pass-through JSON
					// and this check does not change that: it reads one field
					// to validate it, and still interprets none of them.
					ids := spriteSymbolIDs(t)
					for _, group := range []struct {
						kind  string
						cards []map[string]any
					}{
						{"card", b.EmptyState.Cards},
						{"protocol pill", b.EmptyState.ProtocolPills},
					} {
						for _, card := range group.cards {
							icon, _ := card["icon"].(string)
							id, _ := card["id"].(string)
							if strings.TrimSpace(icon) == "" {
								t.Errorf("%s %q declares no icon", group.kind, id)
								continue
							}
							if !ids[icon] {
								t.Errorf("%s %q icon %q is not a symbol in the sprite — it renders as an empty box; add the glyph to web/public/icons/core-icons.svg", group.kind, id, icon)
							}
						}
					}
				}},
			} {
				t.Run(tc.name, tc.check)
			}
		})
	}
}
