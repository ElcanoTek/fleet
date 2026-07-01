package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

func TestBrowserSnippet(t *testing.T) {
	cases := []struct {
		name    string
		params  BrowserParams
		wantErr bool
		// contains is a fragment the generated snippet must include.
		contains string
	}{
		{"navigate", BrowserParams{Action: "navigate", URL: "https://example.com"}, false, `_fleet_nav("https://example.com")`},
		{"navigate missing url", BrowserParams{Action: "navigate"}, true, ""},
		{"navigate relative url", BrowserParams{Action: "navigate", URL: "example.com"}, true, ""},
		{"read", BrowserParams{Action: "read"}, false, "_fleet_read()"},
		{"click", BrowserParams{Action: "click", Ref: 3}, false, "_fleet_click(3)"},
		{"click missing ref", BrowserParams{Action: "click"}, true, ""},
		{"type", BrowserParams{Action: "type", Ref: 2, Text: "hi"}, false, `_fleet_type(2, "hi")`},
		{"type missing text", BrowserParams{Action: "type", Ref: 2}, true, ""},
		{"screenshot", BrowserParams{Action: "screenshot"}, false, "_fleet_screenshot()"},
		{"unknown", BrowserParams{Action: "frobnicate"}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snip, err := browserSnippet(tc.params)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got snippet:\n%s", snip)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(snip, browserPreamble) {
				t.Error("snippet must include the session preamble")
			}
			if !strings.Contains(snip, tc.contains) {
				t.Fatalf("snippet missing %q:\n%s", tc.contains, snip)
			}
		})
	}
}

// TestBrowserSnippet_UntrustedInputEscaped ensures a hostile URL/text cannot
// break out of its Python string literal (JSON-encoded, quote-safe).
func TestBrowserSnippet_UntrustedInputEscaped(t *testing.T) {
	hostile := `https://x.test/"); import os; os.system("rm -rf /"); ("`
	snip, err := browserSnippet(BrowserParams{Action: "navigate", URL: hostile})
	if err != nil {
		t.Fatal(err)
	}
	// The quote that would end the literal must be ESCAPED (\"), so the
	// injection stays inert data inside the string.
	if !strings.Contains(snip, `\"); import os`) {
		t.Fatalf("hostile input must be JSON-escaped inside the literal:\n%s", snip)
	}
	// And the call must remain a single _fleet_nav(...) — no bare statement
	// injected at top level.
	if strings.Contains(snip, "\nimport os") || strings.Contains(snip, "os.system(\"rm") {
		t.Fatalf("hostile input produced a top-level statement:\n%s", snip)
	}
}

func TestRunBrowserAction_LockdownRefused(t *testing.T) {
	_, err := runBrowserAction(context.Background(), nil, BrowserConfig{Enabled: true, Lockdown: true}, BrowserParams{Action: "read"})
	if err == nil || !strings.Contains(err.Error(), "BROWSER_UNAVAILABLE") {
		t.Fatalf("lockdown must refuse: %v", err)
	}
}

func TestInterpretBrowserResult(t *testing.T) {
	mk := func(vars map[string]any, stderr string) sandbox.PythonResult {
		return sandbox.PythonResult{Vars: vars, Stderr: stderr}
	}

	// Missing playwright → clear not-installed error.
	if _, err := interpretBrowserResult(mk(nil, "ModuleNotFoundError: No module named 'playwright'")); err == nil ||
		!strings.Contains(err.Error(), "BROWSER_NOT_INSTALLED") {
		t.Fatalf("not-installed detection: %v", err)
	}

	// Session lost → recoverable error.
	if _, err := interpretBrowserResult(mk(map[string]any{
		"_fleet_browser_result": map[string]any{"session_lost": true},
	}, "")); err == nil || !strings.Contains(err.Error(), "BROWSER_SESSION_LOST") {
		t.Fatalf("session-lost detection: %v", err)
	}

	// Action failure surfaces the error.
	if _, err := interpretBrowserResult(mk(map[string]any{
		"_fleet_browser_result": map[string]any{"ok": false, "action": "navigate", "error": "net::ERR_NAME_NOT_RESOLVED"},
	}, "")); err == nil || !strings.Contains(err.Error(), "ERR_NAME_NOT_RESOLVED") {
		t.Fatalf("action error: %v", err)
	}

	// Successful read renders text + numbered elements.
	out, err := interpretBrowserResult(mk(map[string]any{
		"_fleet_browser_result": map[string]any{
			"ok": true, "action": "read", "url": "https://example.com", "title": "Example",
			"text": "Welcome to Example",
			"elements": []any{
				map[string]any{"ref": float64(1), "kind": "link", "text": "More info"},
				map[string]any{"ref": float64(2), "kind": "button", "text": "Sign in"},
			},
		},
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Example", "Welcome to Example", "[1] link: More info", "[2] button: Sign in"} {
		if !strings.Contains(out, want) {
			t.Fatalf("read output missing %q:\n%s", want, out)
		}
	}

	// Screenshot result names the workspace file and says the model didn't get it.
	shot, err := interpretBrowserResult(mk(map[string]any{
		"_fleet_browser_result": map[string]any{"ok": true, "action": "screenshot", "url": "https://x", "screenshot": "browser-1.png"},
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(shot, "browser-1.png") || !strings.Contains(shot, "did not receive") {
		t.Fatalf("screenshot summary: %s", shot)
	}
}
