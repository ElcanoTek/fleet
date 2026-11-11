package httpapi

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// A Go tool decodes its arguments ignoring case and keeps the last spelling,
// so a session pattern reads every spelling of the name it gives: a deny
// matches when any of them matches, an approve only when all of them do.
func TestSessionPatternsReadEverySpellingOfTheArgument(t *testing.T) {
	t.Parallel()

	r := NewSessionApprovalRegistry()
	r.Register("deny", "bash", SessionApprovalPolicy{Mode: "approve"})
	r.Register("deny", "bash", SessionApprovalPolicy{Mode: "deny", Pattern: "command=rm *"})
	for _, raw := range []string{
		`{"command":"rm -rf build"}`,
		`{"Command":"rm -rf build"}`,
		`{"COMMAND":"rm -rf build"}`,
		`{"comm\u0061nd":"rm -rf build"}`,
		`{"command":"ls","Command":"rm -rf build"}`,
		`{"Command":"rm -rf build","command":"ls"}`,
		`{"command":"rm -rf build","command":"ls"}`,
		`{"command":"rm -rf build","COMMAND":null}`,
		`{"command":"rm -rf build","x":1e400}`,
	} {
		if p, ok := r.Match("deny", "bash", raw); !ok || p.Mode != "deny" {
			t.Errorf("%s: policy %+v (ok=%v), want the rm deny", raw, p, ok)
		}
	}
	if p, ok := r.Match("deny", "bash", `{"command":"ls","Command":"pwd"}`); !ok || p.Mode != "approve" {
		t.Errorf("no spelling matches the deny: policy %+v (ok=%v), want the approve-all", p, ok)
	}

	// The pattern's own name is read ignoring case too, with Unicode case
	// folding as encoding/json applies it ("ſ" is an "s").
	r.Register("fold", "mcp_x", SessionApprovalPolicy{Mode: "deny", Pattern: "Session_ID=s-*"})
	for _, raw := range []string{`{"session_id":"s-1"}`, `{"ſession_id":"s-1"}`} {
		if p, ok := r.Match("fold", "mcp_x", raw); !ok || p.Mode != "deny" {
			t.Errorf("%s: policy %+v (ok=%v), want the session_id deny", raw, p, ok)
		}
	}

	r.Register("approve", "bash", SessionApprovalPolicy{Mode: "approve", Pattern: "command=ls*"})
	for _, raw := range []string{`{"command":"ls -la"}`, `{"Command":"ls -la"}`, `{"command":"ls","COMMAND":"ls -la"}`, `{"command":"ls","x":1e400}`} {
		if p, ok := r.Match("approve", "bash", raw); !ok || p.Mode != "approve" {
			t.Errorf("%s: policy %+v (ok=%v), want the ls approve", raw, p, ok)
		}
	}
	for _, raw := range []string{
		`{"command":"ls","Command":"rm -rf build"}`,
		`{"Command":"rm -rf build","command":"ls"}`,
		`{"command":"rm -rf build","command":"ls"}`,
		`{"command":"ls","Command":null}`,
		`{"command":"ls","COMMAND":["rm","-rf","build"]}`,
		`{}`,
		`[{"command":"ls"}]`,
		`{"command":"ls"} {"command":"rm -rf build"}`,
		`not json`,
	} {
		if p, ok := r.Match("approve", "bash", raw); ok {
			t.Errorf("%s: matched %+v; the approve covers only calls whose every spelling matches", raw, p)
		}
	}
}

// playwright_browser reads its action into a string field: a null spelling
// leaves the action another spelling set.
func TestSessionActionPatternsSkipANullSpelling(t *testing.T) {
	t.Parallel()

	r := NewSessionApprovalRegistry()
	r.Register("c", tools.PlaywrightBrowserToolName, SessionApprovalPolicy{Mode: "approve"})
	r.Register("c", tools.PlaywrightBrowserToolName, SessionApprovalPolicy{Mode: "deny", Pattern: "action=start_session"})
	r.Register("c", tools.PlaywrightBrowserToolName, SessionApprovalPolicy{Mode: "deny", Pattern: "action=close_session"})
	if p, ok := r.Match("c", tools.PlaywrightBrowserToolName, `{"action":"live","Action":null}`); !ok || p.Pattern != "action=start_session" {
		t.Errorf("policy %+v (ok=%v), want the start_session deny", p, ok)
	}
	if p, ok := r.Match("c", tools.PlaywrightBrowserToolName, `{"Action":null,"action":null}`); !ok || p.Mode != "approve" {
		t.Errorf("no action is a run: policy %+v (ok=%v), want the approve-all", p, ok)
	}

	r.Register("cs", tools.ConnectorStudioToolName, SessionApprovalPolicy{Mode: "approve"})
	r.Register("cs", tools.ConnectorStudioToolName, SessionApprovalPolicy{Mode: "deny", Pattern: "action=start_copilot_session"})
	if p, ok := r.Match("cs", tools.ConnectorStudioToolName, `{"action":"live_view","ACTION":null}`); !ok || p.Mode != "deny" {
		t.Errorf("connector_studio: policy %+v (ok=%v), want the start_copilot_session deny", p, ok)
	}

	r.Register("run", tools.PlaywrightBrowserToolName, SessionApprovalPolicy{Mode: "approve", Pattern: "action=run"})
	if p, ok := r.Match("run", tools.PlaywrightBrowserToolName, `{"action":null,"ACTION":"live"}`); ok {
		t.Errorf("matched %+v; the call starts a session, not a run", p)
	}
}

// Terminal policies on handler-only cards look an argument up by the name
// the server sends. One the call writes more than once is left out, so no
// terminal pattern can match a spelling the handler does not read.
func TestHandlerOnlyPatternArgumentsLeaveOutARepeatedName(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]map[string]string{
		`{"name":"n","cron":"0 9 * * *","Cron":"* * * * *"}`: {"name": "n"},
		`{"name":"n","cron":"0 9 * * *","cron":"* * * * *"}`: {"name": "n"},
		`{"name":"n","cron":"0 9 * * *","CRON":null}`:        {"name": "n"},
		`{"Name":"n","cron":"0 9 * * *"}`:                    {"Name": "n", "cron": "0 9 * * *"},
	} {
		got := handlerApprovalPatternArgs("schedule_task", raw)
		if len(got) != len(want) {
			t.Errorf("%s: pattern args %v, want %v", raw, got, want)
			continue
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: pattern args %v, want %v", raw, got, want)
			}
		}
	}
	// Every argument repeated: the server still sends pattern_args (empty), so
	// the terminal knows it may match patterns but none matches this card.
	if got := handlerApprovalPatternArgs("schedule_task", `{"cron":"0 9 * * *","CRON":"* * * * *"}`); got == nil || len(got) != 0 {
		t.Errorf("every argument repeated: pattern args %#v, want an empty map", got)
	}
	if got := handlerApprovalPatternArgs("schedule_task", `["cron"]`); got != nil {
		t.Errorf("non-object input: pattern args %v, want none", got)
	}
}

// foldName is the key handlerApprovalPatternArgs counts spellings by: two
// names share it exactly when strings.EqualFold (and encoding/json) call them
// the same name.
func TestFoldNameAgreesWithEqualFold(t *testing.T) {
	t.Parallel()

	names := []string{"cron", "CRON", "Cron", "session_id", "\u017fession_id", "SESSION_ID", "sessionid", "session-id",
		"kind", "\u212aind", "KIND", "\u00e5", "\u00c5", "\u212b", "\u00df", "\u1e9e", "ss", "\u03c3", "\u03c2", "\u03a3", "\u0131", "i", "I", "\u0130", "", "_"}
	for _, a := range names {
		for _, b := range names {
			if got, want := foldName(a) == foldName(b), strings.EqualFold(a, b); got != want {
				t.Errorf("%q vs %q: same foldName %v, EqualFold %v", a, b, got, want)
			}
		}
	}
}
