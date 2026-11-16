package httpapi

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// A session pattern's glob is read by the rule's mode: in a deny, '*' and '?'
// match '/' too, so "command=rm *" covers a command that names a path; in an
// approve they stop at '/', as before.
func TestSessionPatternGlobsReadASlashByMode(t *testing.T) {
	t.Parallel()

	r := NewSessionApprovalRegistry()
	r.Register("deny", "bash", SessionApprovalPolicy{Mode: "approve"})
	r.Register("deny", "bash", SessionApprovalPolicy{Mode: "deny", Pattern: "command=rm *"})
	r.Register("deny", "bash", SessionApprovalPolicy{Mode: "deny", Pattern: "command=cat /etc?passwd"})
	for _, raw := range []string{
		`{"command":"rm -rf /tmp/x"}`,
		`{"command":"rm /"}`,
		`{"command":"rm ./build/out"}`,
		`{"Command":"rm -rf /tmp/x"}`,
		`{"command":"ls","COMMAND":"rm -rf /tmp/x"}`,
		`{"command":"cat /etc/passwd"}`,
	} {
		if p, ok := r.Match("deny", "bash", raw); !ok || p.Mode != "deny" {
			t.Errorf("%s: policy %+v (ok=%v), want a deny", raw, p, ok)
		}
	}
	if p, ok := r.Match("deny", "bash", `{"command":"ls /tmp"}`); !ok || p.Mode != "approve" {
		t.Errorf("ls /tmp: policy %+v (ok=%v), want the approve-all", p, ok)
	}

	r.Register("approve", "bash", SessionApprovalPolicy{Mode: "approve", Pattern: "command=ls *"})
	r.Register("approve", "web_fetch", SessionApprovalPolicy{Mode: "approve", Pattern: "url=https://*.example.com/*"})
	for _, c := range [][2]string{
		{"bash", `{"command":"ls -la"}`},
		{"web_fetch", `{"url":"https://docs.example.com/guide"}`},
	} {
		if p, ok := r.Match("approve", c[0], c[1]); !ok || p.Mode != "approve" {
			t.Errorf("%s %s: policy %+v (ok=%v), want the approve", c[0], c[1], p, ok)
		}
	}
	for _, c := range [][2]string{
		{"bash", `{"command":"ls /tmp"}`},
		{"web_fetch", `{"url":"https://docs.example.com/guide/install"}`},
		{"web_fetch", `{"url":"https://other.net/x.example.com/y"}`},
	} {
		if p, ok := r.Match("approve", c[0], c[1]); ok {
			t.Errorf("%s %s: matched %+v; an approve's '*' stops at '/'", c[0], c[1], p)
		}
	}

	// The browser tools' action aliases still match (an action has no '/').
	r.Register("browser", tools.PlaywrightBrowserToolName, SessionApprovalPolicy{Mode: "deny", Pattern: "action=start_*"})
	if p, ok := r.Match("browser", tools.PlaywrightBrowserToolName, `{"action":"live"}`); !ok || p.Mode != "deny" {
		t.Errorf("live: policy %+v (ok=%v), want the start_* deny", p, ok)
	}

	// A malformed glob matches nothing, as before.
	r.Register("bad", "bash", SessionApprovalPolicy{Mode: "deny", Pattern: "command=rm [a"})
	r.Register("bad", "bash", SessionApprovalPolicy{Mode: "approve", Pattern: "command=ls [a"})
	for _, raw := range []string{`{"command":"rm [a"}`, `{"command":"ls [a"}`} {
		if p, ok := r.Match("bad", "bash", raw); ok {
			t.Errorf("%s: malformed glob matched %+v", raw, p)
		}
	}
}
