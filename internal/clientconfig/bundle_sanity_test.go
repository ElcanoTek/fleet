package clientconfig_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	clientconfig "github.com/ElcanoTek/fleet/internal/clientconfig"
)

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
			} {
				t.Run(tc.name, tc.check)
			}
		})
	}
}
