package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// Governed browser tool (#503, v1 — DOM-first, mode-1 "in-sandbox browser for
// untrusted/public web"). The browser is a Playwright(sync) session that lives
// in the SAME persistent per-conversation Python kernel as run_python (#213):
// module-level handles (_fleet_browser/_fleet_page) survive across tool calls
// exactly like REPL variables, so it inherits the mandatory sandbox's egress
// posture, resource cgroups, and --init zombie-reaping for free — no new
// runtime.
//
// SECURITY MODEL (validated design): the boundary is EGRESS, not per-action
// approval. A submit/click approval card is theater — click can submit forms,
// and the real exfiltration channel is navigate-with-query-params — so v1 has
// NO approval cards and instead:
//   - REFUSES in lockdown (network=none: a browser is useless and the hard seal
//     must never be weakened) with a clear per-call error;
//   - is meant to run under the ALLOWLISTED egress posture (#211), which
//     structurally kills the exfil channel: navigate to a non-allowlisted host
//     fails at the host CONNECT proxy regardless of what an injected page says.
//     Under OPEN egress the tool still works but logs are the operator's
//     responsibility (documented reduced-safety) — a deployment that wants the
//     browser should run allowlisted.
//
// CREDENTIALS: none in v1. No login flows, no host-side secret fill. Screenshots
// are written to the conversation workspace for the HUMAN to view (the existing
// workspace-image path) and are NOT fed to the model — v1 is DOM-only, so there
// is no vision-model prompt-injection surface. Login-walled sites and a
// human-authorized local browser operator are the documented mode-2 follow-on.

// BrowserConfig gates the tool for one turn.
type BrowserConfig struct {
	// Enabled mirrors config.BrowserEnabled: the tool is registered only when
	// true (the sandbox image must carry Chromium+Playwright).
	Enabled bool
	// Lockdown is the turn's lockdown posture; true → every action returns a
	// clear per-call error (the browser can reach nothing and must not imply
	// otherwise).
	Lockdown bool
}

// BrowserParams is the tool's typed input: one action against the persistent
// session.
type BrowserParams struct {
	// Action: navigate | read | click | type | screenshot.
	Action string `json:"action" description:"One of: navigate (go to a URL), read (extract page text + a numbered list of interactive elements), click (click an interactive element by its number), type (type text into an element by its number), screenshot (save a PNG of the page to the workspace for the human to view)."`
	// URL for navigate.
	URL string `json:"url,omitempty" description:"For action=navigate: the absolute http(s) URL to open."`
	// Ref is the interactive-element number (from a prior read) for click/type.
	Ref int `json:"ref,omitempty" description:"For action=click/type: the number of the interactive element, as shown in the most recent read output."`
	// Text for type.
	Text string `json:"text,omitempty" description:"For action=type: the text to type into the referenced element."`
}

const browserDescription = `Operate a real web browser INSIDE the sandbox to use websites that have no API (public/untrusted web).

The browser session persists across calls in this conversation, like your Python REPL variables. A typical flow: navigate to a URL, read the page (you get its text plus a NUMBERED list of interactive elements), then click(number) or type(number, text), then read again to see the result.

Actions:
- navigate {url}      open an absolute http(s) URL
- read                extract the current page's text + a numbered list of links/buttons/inputs
- click {ref}         click the interactive element with that number (from the latest read)
- type {ref, text}    type text into the input with that number
- screenshot          save a PNG of the current page to the workspace (the USER can view it; you will NOT receive the image)

Notes:
- Always read after navigating or acting so the element numbers are current.
- This is DOM-first: canvas/WebGL apps (e.g. map tiles) won't expose readable elements.
- Do NOT enter passwords or secrets — logging into sites is not supported in this version.
- If a page needs a login/CAPTCHA/2FA you cannot pass, say so and stop.`

// NewBrowserTool builds the governed browser tool bound to the turn's sandbox
// (the persistent Python kernel it shares with run_python).
func NewBrowserTool(sb *sandbox.Sandbox, cfg BrowserConfig) fantasy.AgentTool {
	return fantasy.NewAgentTool("browser", browserDescription,
		func(ctx context.Context, params BrowserParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			out, err := runBrowserAction(ctx, sb, cfg, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(out), nil
		})
}

// runBrowserAction validates + dispatches one action through the kernel.
func runBrowserAction(ctx context.Context, sb *sandbox.Sandbox, cfg BrowserConfig, params BrowserParams) (string, error) {
	if cfg.Lockdown {
		return "BROWSER_UNAVAILABLE: the browser is disabled in lockdown mode (the sandbox is network-sealed). Use web_fetch/smart_search for read-only web access, or run this conversation without lockdown.", nil
	}
	if sb == nil {
		return "", fmt.Errorf("browser requires a sandbox; pool.Take returned nil or was bypassed")
	}
	snippet, err := browserSnippet(params)
	if err != nil {
		return "", err
	}
	res, err := sb.RunPython(ctx, sandbox.PythonRequest{
		Code:         snippet,
		ReturnVars:   []string{"_fleet_browser_result"},
		Timeout:      browserActionTimeout,
		WorkspaceDir: workspaceDirFromContext(ctx),
	})
	if err != nil {
		return "", fmt.Errorf("browser action failed: %w", err)
	}
	return interpretBrowserResult(res)
}

const browserActionTimeout = browserTimeoutSeconds

// browserTimeoutSeconds bounds one browser action (page loads can be slow).
const browserTimeoutSeconds = 90 * 1e9 // 90s as time.Duration

// browserSnippet renders the Playwright(sync) snippet for one action. It is a
// pure function (unit-tested without a browser): it emits Python that ensures a
// module-level session exists, performs the action, and assigns a JSON result
// to `_fleet_browser_result`. Every value that reaches Python is passed via a
// JSON-encoded literal (json.dumps-safe) so a hostile URL/text can never break
// out of its string.
func browserSnippet(params BrowserParams) (string, error) {
	action := strings.ToLower(strings.TrimSpace(params.Action))
	var b strings.Builder
	b.WriteString(browserPreamble)
	switch action {
	case "navigate":
		url := strings.TrimSpace(params.URL)
		if url == "" {
			return "", fmt.Errorf("browser navigate: url is required")
		}
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return "", fmt.Errorf("browser navigate: url must be an absolute http(s) URL")
		}
		fmt.Fprintf(&b, "_fleet_nav(%s)\n", pyStr(url))
	case "read":
		b.WriteString("_fleet_read()\n")
	case "click":
		if params.Ref <= 0 {
			return "", fmt.Errorf("browser click: ref (a positive element number from the latest read) is required")
		}
		fmt.Fprintf(&b, "_fleet_click(%d)\n", params.Ref)
	case "type":
		if params.Ref <= 0 {
			return "", fmt.Errorf("browser type: ref (a positive element number from the latest read) is required")
		}
		if params.Text == "" {
			return "", fmt.Errorf("browser type: text is required")
		}
		fmt.Fprintf(&b, "_fleet_type(%d, %s)\n", params.Ref, pyStr(params.Text))
	case "screenshot":
		b.WriteString("_fleet_screenshot()\n")
	default:
		return "", fmt.Errorf("browser: unknown action %q (want navigate|read|click|type|screenshot)", params.Action)
	}
	return b.String(), nil
}

// pyStr renders a Go string as a Python string literal safely (JSON strings are
// valid Python literals for the characters we care about; both use \" \\ \n
// escapes and \uXXXX).
func pyStr(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// browserResult is the JSON shape the snippet assigns to _fleet_browser_result.
type browserResult struct {
	OK         bool          `json:"ok"`
	Action     string        `json:"action"`
	URL        string        `json:"url,omitempty"`
	Title      string        `json:"title,omitempty"`
	Text       string        `json:"text,omitempty"`
	Elements   []browserElem `json:"elements,omitempty"`
	Screenshot string        `json:"screenshot,omitempty"`
	Error      string        `json:"error,omitempty"`
	// SessionLost signals the kernel restarted (page handle dead) — a
	// recoverable state: navigate again.
	SessionLost bool `json:"session_lost,omitempty"`
}

type browserElem struct {
	Ref  int    `json:"ref"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// interpretBrowserResult turns the kernel's raw response into a compact,
// model-facing summary. A missing Playwright/Chromium surfaces as a clear
// "not installed" message rather than a stack trace.
func interpretBrowserResult(res sandbox.PythonResult) (string, error) {
	raw := ""
	if res.Vars != nil {
		if v, ok := res.Vars["_fleet_browser_result"]; ok {
			if b, err := json.Marshal(v); err == nil {
				raw = string(b)
			}
		}
	}
	if raw == "" {
		// The snippet failed before assigning a result (import error / crash).
		stderr := res.Stderr + res.Error
		if strings.Contains(stderr, "playwright") || strings.Contains(stderr, "No module named") {
			return "BROWSER_NOT_INSTALLED: the sandbox image has no Chromium/Playwright. The browser tool needs the optional browser layer in the client-config Containerfile (see docs/BROWSER.md).", nil
		}
		if strings.TrimSpace(stderr) == "" {
			return "", fmt.Errorf("browser action produced no result (kernel returned nothing)")
		}
		return "", fmt.Errorf("browser action error: %s", truncateForModel(stderr, 800))
	}
	var r browserResult
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return "", fmt.Errorf("browser: could not parse result: %w", err)
	}
	if r.SessionLost {
		return "BROWSER_SESSION_LOST: the browser session ended (the kernel restarted). Navigate again to start a fresh session.", nil
	}
	if !r.OK {
		if r.Error == "" {
			r.Error = "unknown error"
		}
		return "", fmt.Errorf("browser %s failed: %s", r.Action, truncateForModel(r.Error, 800))
	}
	return formatBrowserResult(&r), nil
}

// formatBrowserResult renders a successful result for the model.
func formatBrowserResult(r *browserResult) string {
	var b strings.Builder
	switch r.Action {
	case "navigate":
		fmt.Fprintf(&b, "Opened %s\nTitle: %s\n(Use action=read to see the page content and interactive elements.)", r.URL, r.Title)
	case "screenshot":
		fmt.Fprintf(&b, "Saved a screenshot to the workspace: %s (the user can view it; you did not receive the image). Current page: %s", r.Screenshot, r.URL)
	case "click", "type":
		fmt.Fprintf(&b, "Done (%s). Current page: %s — %s\n(Use action=read to see the updated page.)", r.Action, r.Title, r.URL)
	default: // read
		fmt.Fprintf(&b, "Page: %s\nURL: %s\n\n", r.Title, r.URL)
		if txt := strings.TrimSpace(r.Text); txt != "" {
			b.WriteString("--- text ---\n")
			b.WriteString(truncateForModel(txt, 6000))
			b.WriteString("\n\n")
		}
		if len(r.Elements) > 0 {
			b.WriteString("--- interactive elements (use the number as ref) ---\n")
			for _, e := range r.Elements {
				fmt.Fprintf(&b, "[%d] %s: %s\n", e.Ref, e.Kind, truncateForModel(e.Text, 120))
			}
		}
	}
	return b.String()
}

func truncateForModel(s string, maxChars int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxChars {
		return s
	}
	return s[:maxChars] + "…[truncated]"
}

// workspaceDirFromContext resolves the per-conversation workspace dir (where a
// screenshot lands so the workspace-image path can render it), matching the
// run_python tool's resolution.
func workspaceDirFromContext(ctx context.Context) string {
	if convID := ConversationIDFromContext(ctx); convID != "" {
		if dir, err := EnsureWorkspaceDir(convID); err == nil {
			return dir
		}
	}
	return ""
}
