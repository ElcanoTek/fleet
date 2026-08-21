package clientconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The browserbase pack (#987) is the first built-in skill whose whole job is to
// coordinate with a per-user MCP connector, and four of its instructions are
// load-bearing in ways a well-meaning edit would quietly undo. Each one below is
// a failure a user would actually hit, so they are pinned rather than trusted to
// survive review.
func TestBuiltinSkillBrowserbaseCoversFailureModes(t *testing.T) {
	merged, err := materializeMergedSkills(filepath.Join(t.TempDir(), "no-bundle-skills"), true, nil)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(merged, "browserbase", "SKILL.md"))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	body := string(raw)

	for _, tc := range []struct {
		name   string
		needle string
		why    string
	}{
		{
			name:   "does not trust the live registry",
			needle: "MCP Tools (live registry)",
			why: "the prompt's live-registry section is built from the shared catalog and is blind to " +
				"per-user hosted connectors, so it can deny tools the model actually holds. Without this " +
				"warning the agent refuses a working connector.",
		},
		{
			name:   "tool names are not hardcoded",
			needle: "_start",
			why: "connector tool names are mcp_<connection-name>_<tool>, and the connection name is " +
				"whatever the user typed. Matching on the suffix is the only reliable instruction.",
		},
		{
			name:   "ends the turn at the handoff",
			needle: "End your turn",
			why: "interactive chat cannot block on a human (ask/sleep are scheduled-only, approval cards " +
				"give no new turn), so the flow is two turns by construction. An agent that waits spins.",
		},
		{
			name:   "warns the link is a capability",
			needle: "can drive that browser",
			why: "the live-view URL needs no login, so whoever holds it controls the session — including " +
				"whatever the user just logged into.",
		},
		{
			name:   "offers the key-free fallback",
			needle: "browserbase.com/sessions",
			why: "when the operator has not set BROWSERBASE_API_KEY there is no live_view tool, and the " +
				"dashboard is the path that needs nothing from this server.",
		},
		{
			name:   "resumes with an explicit session id",
			needle: "same session id",
			why: "fleet may open a fresh MCP transport between turns, so only an explicit session id " +
				"reliably reattaches to the browser the user logged into.",
		},
		{
			name:   "does not tear down early",
			needle: "confirmed they are finished",
			why: "ending the session kills the user's live view mid-task, and tidying up is exactly what " +
				"a well-behaved model does unprompted.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(body, tc.needle) {
				t.Errorf("browserbase SKILL.md no longer mentions %q.\nWhy it matters: %s", tc.needle, tc.why)
			}
		})
	}

	// The pack deliberately ships no bundled files: it needs no script and no
	// template, and staying prose-only keeps it clear of the sandbox-mount and
	// vendored-asset questions the bento pack has to answer. If that ever
	// changes, the reference-path test from builtin_skills_bento_test.go should
	// be ported alongside it.
	entries, err := os.ReadDir(filepath.Join(merged, "browserbase"))
	if err != nil {
		t.Fatalf("read pack dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "SKILL.md" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("pack contents = %v; expected SKILL.md only. If bundled files were added on purpose, "+
			"port the referenced-path assertions from the bento pack's test so prose pointing at a "+
			"renamed file fails CI.", names)
	}
}

// The catalog entry's URL must keep carrying keepAlive=true. Without it
// Browserbase ends the session when the transport closes, which is exactly the
// moment the browserbase skill hands a live-view link to the user and ends the
// turn — so the login they are about to complete would be thrown away.
// internal/mcp.TestWithQueryParamPreservesExistingQuery pins the other half:
// that attaching the API key does not clobber this parameter.
func TestBrowserbaseCatalogEntryKeepsSessionsAlive(t *testing.T) {
	entries, err := loadBuiltinRemoteCatalog()
	if err != nil {
		t.Fatalf("load builtin remote catalog: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Name != "browserbase" {
			continue
		}
		found = true
		if !strings.Contains(e.URL, "keepAlive=true") {
			t.Errorf("browserbase url = %q; it must carry keepAlive=true so a hosted session "+
				"outlives the turn in which the agent hands the user a live-view link", e.URL)
		}
		if e.Auth != "api_key" || e.APIKeyQuery == "" {
			t.Errorf("browserbase auth = %q / api_key_query = %q; the live-view flow assumes the "+
				"key is attached per-request as a query parameter", e.Auth, e.APIKeyQuery)
		}
	}
	if !found {
		t.Fatal("browserbase entry missing from the built-in remote catalog")
	}
}
