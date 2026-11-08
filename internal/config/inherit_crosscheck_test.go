package config_test

import (
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/httpapi"
	"github.com/ElcanoTek/fleet/internal/tools"
	"github.com/ElcanoTek/fleet/internal/webui"
)

// inheritReader reads what the code actually uses for a setting with an
// inherited default (DefaultFrom): a config.Load field, or the package
// reader that resolves it at its point of use.
type inheritReader struct {
	read func(t *testing.T) string
	// sample is a value to set the setting (or an ancestor) to.
	sample string
}

func loadField(f func(*config.Config) string) func(t *testing.T) string {
	return func(t *testing.T) string {
		t.Helper()
		cfg, err := config.Load("")
		if err != nil {
			t.Fatal(err)
		}
		return f(cfg)
	}
}

// inheritReaders maps every registry entry with DefaultFrom (and every
// ancestor the scenarios set) to its real reader.
var inheritReaders = map[string]inheritReader{
	"FLEET_TITLE_MODEL":           {loadField(func(c *config.Config) string { return c.TitleModel }), "vendor/title-model"},
	"FLEET_METADATA_MODEL":        {loadField(func(c *config.Config) string { return c.MetadataModel }), "vendor/metadata-model"},
	"FLEET_ERROR_ANALYSIS_MODEL":  {loadField(func(c *config.Config) string { return c.ErrorAnalysisModel }), "vendor/error-model"},
	"FLEET_MEMORY_MODEL":          {loadField(func(c *config.Config) string { return c.MemoryModel }), "vendor/memory-model"},
	"FLEET_RECURRING_TASK_MODEL":  {loadField(func(c *config.Config) string { return c.RecurringTaskModel }), "vendor/recurring-model"},
	"FLEET_LIBRARY_PROMPT_MODEL":  {loadField(func(c *config.Config) string { return c.LibraryPromptModel }), "vendor/library-model"},
	"FLEET_SANDBOX_WARM_SIZE":     {loadField(func(c *config.Config) string { return strconv.Itoa(agent.WarmPoolSize(c)) }), "3"},
	"FLEET_MAX_CONCURRENT_AGENTS": {loadField(func(c *config.Config) string { return strconv.Itoa(c.MaxConcurrentAgents) }), "5"},
	"FLEET_UPDATE_DRAIN_FILE":     {func(*testing.T) string { return httpapi.UpdateDrainFile() }, "/srv/drain/update"},
	"FLEET_DRAIN_FILE":            {func(*testing.T) string { return httpapi.UpdateDrainFile() }, "/srv/drain/legacy"},
	"FLEET_ENV_FILE":              {func(*testing.T) string { return os.Getenv("FLEET_ENV_FILE") }, "/srv/fleet/custom.env"},
	"FLEET_AUDIT_DIR":             {func(*testing.T) string { return tools.AuditDir() }, "/srv/audit"},
	"FLEET_DATA_DIR":              {loadField(func(c *config.Config) string { return c.DataDir }), "/srv/data"},
	"FLEET_OIDC_REDIRECT_URI": {func(*testing.T) string {
		c := webui.ConfigFromEnv()
		if c.OIDC != nil && c.OIDC.RedirectURI != "" {
			return c.OIDC.RedirectURI
		}
		if c.PublicOrigin == nil {
			return "" // derived per request: not a configured value
		}
		return c.PublicOrigin.ResolveReference(&url.URL{Path: "/api/auth/oidc/callback"}).String()
	}, "https://sso.example.com/cb"},
	"FLEET_PUBLIC_BASE_URL": {loadField(func(c *config.Config) string { return c.PublicBaseURL }), "https://fleet.example.com"},
}

// clearRegistryEnv unsets every registered spelling for the test.
func clearRegistryEnv(t *testing.T) {
	t.Helper()
	for _, e := range envreg.All() {
		for _, s := range e.Spellings() {
			t.Setenv(s, "")
			_ = os.Unsetenv(s)
		}
	}
}

// ancestors returns e's DefaultFrom chain, nearest first.
func ancestors(e envreg.Entry) []string {
	var out []string
	for p := e.DefaultFrom; p != ""; {
		out = append(out, p)
		pe, _ := envreg.Find(p)
		p = pe.DefaultFrom
	}
	return out
}

// TestInheritedDefaultsMatchTheReaders — Codex round 3 (#2057): for every
// registry entry with DefaultFrom, explain's configured value must be what
// the code actually uses, with nothing set, with each ancestor set, and with
// the setting itself set. Secrets are compared through the reader directly
// (explain redacts them) and must still report the inheritance.
func TestInheritedDefaultsMatchTheReaders(t *testing.T) {
	t.Chdir(t.TempDir())
	checked := 0
	for _, e := range envreg.All() {
		if e.DefaultFrom == "" {
			continue
		}
		r, ok := inheritReaders[e.Name]
		if !ok {
			t.Errorf("%s inherits a default but has no reader in inheritReaders", e.Name)
			continue
		}
		anc := ancestors(e)
		scenarios := make([]string, 0, len(anc)+2)
		scenarios = append(scenarios, "") // nothing set
		scenarios = append(scenarios, anc...)
		scenarios = append(scenarios, e.Name)
		for _, setName := range scenarios {
			t.Run(e.Name+"/set="+setName, func(t *testing.T) {
				clearRegistryEnv(t)
				// SSO configured, so the gateway builds its OIDC config.
				t.Setenv("FLEET_OIDC_ISSUER", "https://idp.example.com")
				t.Setenv("FLEET_OIDC_CLIENT_ID", "client")
				t.Setenv("FLEET_OIDC_CLIENT_SECRET", "placeholder")
				t.Setenv("FLEET_OIDC_GENERIC", "1")
				if setName != "" {
					sr, ok := inheritReaders[setName]
					if !ok {
						t.Fatalf("no sample value for %s", setName)
					}
					t.Setenv(setName, sr.sample)
				}
				rep, err := config.Explain(config.ExplainOptions{Keys: []string{e.Name}})
				if err != nil {
					t.Fatal(err)
				}
				x := rep.Settings[0]
				got := r.read(t)
				want := x.Value
				if strings.HasPrefix(want, "(") {
					want = "" // a prose default: not a configured value
				}
				if e.Secret {
					if x.Value != "" && x.Value != config.Redacted {
						t.Fatalf("secret printed: %q", x.Value)
					}
					want = ""
					if setName != "" {
						want = inheritReaders[setName].sample
					}
				}
				if got != want {
					t.Fatalf("explain reports %q (set=%v inherited=%v) but the reader uses %q", x.Value, x.Set, x.InheritedFrom, got)
				}
				if setName != "" && setName != e.Name && (len(x.InheritedFrom) == 0 || x.InheritedFrom[len(x.InheritedFrom)-1] != setName) {
					t.Errorf("inherited_from %v does not end at %s", x.InheritedFrom, setName)
				}
				checked++
			})
		}
	}
	if checked < 30 {
		t.Fatalf("only %d scenarios ran", checked)
	}
}
