package httpapi

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// TestSessionActionPatternsMatchEverySpelling: a session pattern on a browser
// tool's action applies to every spelling of that action, so "deny
// start_session" also stops "live", and a pattern written with an alias stands
// for its canonical action. Other tools match their args as written.
func TestSessionActionPatternsMatchEverySpelling(t *testing.T) {
	t.Parallel()
	r := NewSessionApprovalRegistry()
	r.Register("c1", tools.PlaywrightBrowserToolName, SessionApprovalPolicy{Mode: "approve"})
	r.Register("c1", tools.PlaywrightBrowserToolName, SessionApprovalPolicy{Mode: "deny", Pattern: "action=start_session"})
	for _, action := range []string{"start_session", "live", "COPILOT", " open_session "} {
		p, ok := r.Match("c1", tools.PlaywrightBrowserToolName, `{"action":"`+action+`"}`)
		if !ok || p.Mode != "deny" {
			t.Errorf("action %q: policy %+v (ok=%v), want the start_session deny", action, p, ok)
		}
	}
	if p, ok := r.Match("c1", tools.PlaywrightBrowserToolName, `{"action":"step"}`); !ok || p.Mode != "approve" {
		t.Errorf("action step: policy %+v (ok=%v), want the approve-all", p, ok)
	}

	r.Register("c2", tools.ConnectorStudioToolName, SessionApprovalPolicy{Mode: "deny", Pattern: "action=live_view"})
	if p, ok := r.Match("c2", tools.ConnectorStudioToolName, `{"action":"start_copilot_session"}`); !ok || p.Mode != "deny" {
		t.Errorf("an alias in the pattern does not stand for its action: %+v (ok=%v)", p, ok)
	}

	// The action as the tool reads it: any key case, the last duplicate, and
	// playwright_browser's default when it is missing.
	for _, raw := range []string{`{"ACTION":"live"}`, `{"Action":"start_session"}`, `{"action":"steps","ACTION":"live"}`} {
		if p, ok := r.Match("c1", tools.PlaywrightBrowserToolName, raw); !ok || p.Mode != "deny" {
			t.Errorf("%s: policy %+v (ok=%v), want the start_session deny", raw, p, ok)
		}
	}
	r.Register("c4", tools.PlaywrightBrowserToolName, SessionApprovalPolicy{Mode: "approve"})
	r.Register("c4", tools.PlaywrightBrowserToolName, SessionApprovalPolicy{Mode: "deny", Pattern: "action=run"})
	for _, raw := range []string{`{"url":"https://a.example.com/"}`, `{"action":null}`, `{"action":""}`, `{"Action":"browse"}`} {
		if p, ok := r.Match("c4", tools.PlaywrightBrowserToolName, raw); !ok || p.Mode != "deny" {
			t.Errorf("%s: policy %+v (ok=%v), want the run deny", raw, p, ok)
		}
	}

	r.Register("c3", "mcp_pages_deploy_page", SessionApprovalPolicy{Mode: "deny", Pattern: "action=start_session"})
	if _, ok := r.Match("c3", "mcp_pages_deploy_page", `{"action":"live"}`); ok {
		t.Error("a non-browser tool's pattern matched an alias it does not have")
	}
}
