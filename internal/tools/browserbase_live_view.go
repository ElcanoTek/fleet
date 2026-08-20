package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/ElcanoTek/fleet/internal/redact"
)

// browserbase_live_view turns a Browserbase session id into a URL a HUMAN can
// open to watch and drive that hosted browser — the piece the Browserbase MCP
// connector does not provide. Its six tools (`start`, `end`, `navigate`, `act`,
// `observe`, `extract`) let the agent drive a session, but `start` returns only
// a session id: the viewer URL comes from an authenticated call to
// `GET /v1/sessions/{id}/debug`, and by invariant the credential for that can
// never enter the sandbox or the model context.
//
// So this is a host-side control-plane operation, in the "host network /
// brokered fetch" exception class already enumerated in ADR-0036 alongside
// web_fetch, download_url, generate_image and the search tools: one
// authenticated GET to a fixed public vendor host, no model-authored code, no
// sandbox bypass. It drives no browser and renders no page — driving stays with
// the connector, per ADR-0044.
//
// The credential is resolved CONNECTOR-FIRST: the caller injects a
// BrowserbaseKeyFunc that unseals the running user's own Browserbase connector
// key host-side, so pasting the key once in Settings → Connections is enough and
// the capability is scoped to that user. BROWSERBASE_API_KEY from the host env is
// the fallback, for paths with no per-user connection (scheduled runs) and for
// operators who prefer a box-wide key.
//
// Connector-first is what removes the failure mode this tool would otherwise
// have: two independently-configured keys can belong to different Browserbase
// projects, and then minting 404s for a session that demonstrably exists.
// browserbaseStatusError still names that case, because the env fallback can
// still produce it.

// browserbaseAPIBase is the vendor API root. A package var so tests can point
// it at an httptest server; production never changes it.
var browserbaseAPIBase = "https://api.browserbase.com"

// browserbaseDialContext is the dial function for this tool's HTTP client.
// Production keeps the SSRF guard shared with web_fetch and download_url; tests
// against loopback httptest servers substitute a plain dialer. (The host is
// fixed and the model supplies only a session id, so this is defense in depth
// rather than a live SSRF surface — but DNS for a vendor host is still someone
// else's infrastructure.)
var browserbaseDialContext = newSSRFGuardedDialer().DialContext

// browserbaseSessionIDPattern bounds what can reach the request path. A session
// id is an opaque vendor token; anything else is refused before a request is
// built, so a malformed id yields one clear message instead of an unintended
// path signed with the operator's key.
var browserbaseSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

const browserbaseHTTPTimeout = 15 * time.Second

// browserbaseLiveViewDescription has to carry the whole protocol on its own.
// The SKILL.md explains this flow in full, but bundle skills are NOT in the
// scheduled-run roster (docs/SKILLS.md) — so in a scheduled task this
// description is all the model gets.
const browserbaseLiveViewDescription = "Turn a Browserbase session id into a live-view URL a human can open to watch and control that hosted browser. " +
	"Use it when a page needs a person: a login form, a captcha, 2FA, or a consent wall the agent cannot clear itself.\n\n" +
	"REQUIRES the Browserbase MCP connector for the browsing itself — this tool only mints the viewer link. " +
	"Call the connector's `_start` tool first and pass the session id it returns. Note the connector's tools are named after the connection the user created (usually `mcp_browserbase_start`, but the prefix is whatever they named it), and the `## MCP Tools (live registry)` section of your system prompt does NOT list per-user hosted connectors — trust your own tool list, not that section.\n\n" +
	"THE RETURNED URL IS A CAPABILITY: it needs no login, so anyone who gets the link can drive that browser session until it ends. Hand it to the user, tell them not to forward it, and do not paste it anywhere it will outlive the session.\n\n" +
	"HANDOFF: in interactive chat you cannot wait for a human — post the link, say what you need done, and END YOUR TURN. Resume on the user's next message by passing the SAME session id to the connector's navigate/act/extract tools; the browser is where they left it. In a scheduled run, deliver the link with `ask` or `notify` instead. Never call the connector's `end` tool while the link is still in the user's hands — that kills their view mid-task.\n\n" +
	"SESSION: pass `session_id` from `_start` when you have it. If you could not read it — some connectors return a screenshot with it, and a tool result carrying binary is suppressed before you see it — just OMIT `session_id` and this tool uses your one running session. It refuses to guess when several are running, so pass the id explicitly then."

// browserbaseDebugResponse is the subset of GET /v1/sessions/{id}/debug this
// tool consumes. Two fields are deliberately absent:
//
//   - wsUrl: the raw CDP endpoint, a full remote-control channel strictly
//     stronger than the viewer URL. Never surfaced.
//   - pages[].title / pages[].url: attacker-controlled page content. Returning
//     them would inject untrusted web text into the model's context at the exact
//     moment it is following instructions about handing over a capability link.
//     Only the COUNT is reported.
type browserbaseDebugResponse struct {
	DebuggerFullscreenURL string `json:"debuggerFullscreenUrl"`
	Pages                 []struct {
		ID string `json:"id"`
	} `json:"pages"`
}

// BrowserbaseLiveViewParams are the typed parameters for browserbase_live_view.
type BrowserbaseLiveViewParams struct {
	SessionID string `json:"session_id,omitempty" description:"The Browserbase session id from the connector's _start tool. OPTIONAL: omit it and the tool uses your one running session, which is the way through when the connector's own output was suppressed."`
}

// browserbaseSession is the subset of GET /v1/sessions this tool consumes.
type browserbaseSession struct {
	ID        string `json:"id"`
	CreatedAt string `json:"createdAt"`
}

// BrowserbaseKeyFunc unseals the Browserbase connector credential belonging to
// the user running THIS turn.
//
// The caller must return nil unless a key is genuinely reachable — the user has a
// Browserbase connection AND this conversation has it switched on. A non-nil
// func is what registers the tool, so returning one speculatively would put a
// permanently-failing tool in front of every user, and would let the credential
// (and session enumeration) escape the per-conversation connector gate.
type BrowserbaseKeyFunc func(ctx context.Context) (string, error)

// BrowserbaseLiveViewAvailable reports whether a live-view URL can be minted on
// this turn: either the caller supplied a reachable connector key, or the
// operator set a box-wide BROWSERBASE_API_KEY.
func BrowserbaseLiveViewAvailable(connectorReachable bool) bool {
	return connectorReachable || strings.TrimSpace(os.Getenv("BROWSERBASE_API_KEY")) != ""
}

// NewBrowserbaseLiveViewTool returns the tool, or nil when this deployment has no
// way to mint a URL.
//
// Returning nil rather than a tool that always errors is the house rule for a
// capability that is not configured: "absent handlers → the tools aren't
// registered, so the model never sees a capability it can't use"
// (internal/scheduledrun, ask/notify). It also keeps a tool definition out of the
// cacheable prompt prefix on deployments that cannot use it at all, which matters
// because the interactive roster already runs near the tool-disclosure threshold.
func NewBrowserbaseLiveViewTool(keyFn BrowserbaseKeyFunc) fantasy.AgentTool {
	if !BrowserbaseLiveViewAvailable(keyFn != nil) {
		return nil
	}
	return fantasy.NewAgentTool("browserbase_live_view", browserbaseLiveViewDescription,
		func(ctx context.Context, params BrowserbaseLiveViewParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			text, isErr := runBrowserbaseLiveView(ctx, keyFn, params)
			if isErr {
				return fantasy.NewTextErrorResponse(text), nil
			}
			return fantasy.NewTextResponse(text), nil
		})
}

// resolveBrowserbaseKey prefers the running user's own connector credential and
// falls back to the host env. Connector-first keeps the capability per-user and
// means one paste in Settings → Connections is enough; the env fallback covers
// paths with no per-user connection, such as scheduled runs. Read per call, so a
// rotation on either side takes effect without a restart.
// It also reports PROVENANCE, which is a safety input rather than bookkeeping:
// a per-user connector key scopes everything it can reach to that user's own
// Browserbase project, while the box-wide env key reaches every session in a
// project shared by every user of this fleet. Only the former may auto-resolve a
// session (see browserbaseResolveRunningSession).
func resolveBrowserbaseKey(ctx context.Context, keyFn BrowserbaseKeyFunc) (key string, fromConnector bool) {
	if keyFn != nil {
		if k, err := keyFn(ctx); err == nil {
			if k = strings.TrimSpace(k); k != "" {
				return k, true
			}
		}
		// A resolver error is not fatal: the env key may still be set.
		// Deliberately not logged — the value is a credential and the caller
		// reports the outcome.
	}
	return strings.TrimSpace(os.Getenv("BROWSERBASE_API_KEY")), false
}

// browserbaseHTTPClient is the shared client for both vendor calls. Redirects
// are refused outright: Go re-sends headers across a same-host hop, so a
// vendor-side (or DNS-hijacked) 30x would ship X-BB-API-Key to another origin,
// and neither endpoint legitimately redirects.
func browserbaseHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   browserbaseHTTPTimeout,
		Transport: &http.Transport{DialContext: browserbaseDialContext},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirect refused")
		},
	}
}

// browserbaseGet issues one authenticated GET and returns the body.
func browserbaseGet(ctx context.Context, client *http.Client, apiKey, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(browserbaseAPIBase, "/")+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("X-BB-API-Key", apiKey)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return body, resp.StatusCode, err
}

// browserbaseResolveRunningSession finds the session to mint for when the caller
// gave no id. It resolves ONLY an unambiguous single running session and refuses
// to guess otherwise, because guessing wrong means handing a person a live,
// controllable view of a browser that is not theirs — on a shared box using the
// env-key fallback, potentially someone else's.
func browserbaseResolveRunningSession(ctx context.Context, client *http.Client, apiKey string) (string, string) {
	body, status, err := browserbaseGet(ctx, client, apiKey, "/v1/sessions?status=RUNNING")
	if err != nil {
		return "", "BROWSERBASE_UNAVAILABLE: could not reach the Browserbase API to find your running session. " +
			"Do NOT retry more than once — report it and offer the dashboard fallback at https://www.browserbase.com/sessions."
	}
	// Deliberately NOT browserbaseStatusError here: that maps 401/403/404 to
	// "no such session", which is the right story for the debug endpoint and the
	// wrong one for a listing — a 401 on /v1/sessions means the key was rejected,
	// and reporting it as a missing session would send the caller hunting for the
	// wrong problem.
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", "BROWSERBASE_KEY_REJECTED: Browserbase rejected this server's API key (HTTP " +
			strconv.Itoa(status) + "). Do NOT retry — tell the user to check the key on their Browserbase " +
			"connection under Settings → Connections."
	default:
		return "", fmt.Sprintf("BROWSERBASE_UNAVAILABLE: could not list running Browserbase sessions (HTTP %d). "+
			"Do NOT retry more than once — pass an explicit session_id, or offer the dashboard fallback at "+
			"https://www.browserbase.com/sessions.", status)
	}
	var sessions []browserbaseSession
	if err := json.Unmarshal(body, &sessions); err != nil {
		return "", "BROWSERBASE_UNAVAILABLE: the Browserbase session list could not be parsed. Do NOT retry — " +
			"pass an explicit session_id, or offer the dashboard fallback at https://www.browserbase.com/sessions."
	}

	live := make([]browserbaseSession, 0, len(sessions))
	for _, sess := range sessions {
		if browserbaseSessionIDPattern.MatchString(strings.TrimSpace(sess.ID)) {
			live = append(live, sess)
		}
	}
	switch len(live) {
	case 0:
		return "", "BROWSERBASE_NO_RUNNING_SESSION: there is no running Browserbase session to show. " +
			"Call the connector's _start tool (then _navigate somewhere) and try again."
	case 1:
		return live[0].ID, ""
	default:
		// Deliberately NOT listing the ids. An explicit session_id is accepted
		// with no ownership check, so printing them would hand over exactly what
		// is needed to bypass this refusal — and they would land in conversation
		// history, which can be shared publicly.
		return "", fmt.Sprintf("BROWSERBASE_AMBIGUOUS_SESSION: %d Browserbase sessions are running, so this tool will not "+
			"guess which one to expose — a wrong guess hands someone control of a browser that is not theirs. Pass the "+
			"session_id the connector's _start tool returned for the session you started, or end the stale ones first.",
			len(live))
	}
}

func runBrowserbaseLiveView(ctx context.Context, keyFn BrowserbaseKeyFunc, params BrowserbaseLiveViewParams) (string, bool) {
	apiKey, fromConnector := resolveBrowserbaseKey(ctx, keyFn)
	if apiKey == "" {
		return "BROWSERBASE_NOT_CONFIGURED: no Browserbase credential is available for this user, so a live-view link cannot be minted. " +
			"Do NOT retry — tell the user to add Browserbase under Settings → Connections (the same key that drives the browser is used to " +
			"mint the link), or to open the session themselves at https://www.browserbase.com/sessions (click the running session; its live " +
			"view uses their own Browserbase login).", true
	}

	client := browserbaseHTTPClient()

	sessionID := strings.TrimSpace(params.SessionID)
	if sessionID == "" && !fromConnector {
		// A box-wide key can see every session in a shared project, so "the one
		// running session" may belong to a different person entirely — and the
		// URL this tool mints drives a browser someone else is logged into.
		// Refuse to pick one rather than risk handing over the wrong browser.
		return "BROWSERBASE_SESSION_ID_REQUIRED: this server is using a shared Browserbase key, so it will not pick a " +
			"session for you — the running one may belong to another user, and this link grants control of it. " +
			"Pass the session_id the connector's _start tool returned. If you could not read it, ask the user to add " +
			"their own Browserbase connection under Settings → Connections, which scopes sessions to them.", true
	}
	if sessionID == "" {
		// No id given. This is the normal path when the connector's own _start
		// output was suppressed before the model could read it: fleet's tool-output
		// boundary discards a result carrying binary (a screenshot), and the session
		// id travels in the same result. Rather than make the model recover an id it
		// never saw, resolve it from the account.
		resolved, msg := browserbaseResolveRunningSession(ctx, client, apiKey)
		if msg != "" {
			return msg, true
		}
		sessionID = resolved
	}
	if !browserbaseSessionIDPattern.MatchString(sessionID) {
		return "BROWSERBASE_BAD_SESSION_ID: session_id must be the opaque id the connector's _start tool returned " +
			"(8-64 characters, letters/digits/dash/underscore). Do NOT retry with a guess — either omit session_id " +
			"entirely to use your running session, or call the connector's _start tool and pass the id it returns.", true
	}

	body, status, err := browserbaseGet(ctx, client, apiKey, "/v1/sessions/"+sessionID+"/debug")
	if err != nil {
		// Never surface the raw transport error: it can echo the request URL.
		return "BROWSERBASE_UNAVAILABLE: could not reach the Browserbase API. Do NOT retry more than once — " +
			"report that the live view could not be minted and offer the dashboard fallback at https://www.browserbase.com/sessions.", true
	}
	if msg, bad := browserbaseStatusError(status, sessionID); bad {
		return msg, true
	}
	var decoded browserbaseDebugResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "BROWSERBASE_UNAVAILABLE: the Browserbase API returned a response this tool could not parse. " +
			"Do NOT retry — report it and offer the dashboard fallback at https://www.browserbase.com/sessions.", true
	}

	liveView := strings.TrimSpace(decoded.DebuggerFullscreenURL)
	if liveView == "" {
		return "BROWSERBASE_LIVE_VIEW_NOT_READY: the session exists but has no live view yet — Browserbase only publishes one " +
			"once a browser client has attached. Call the connector's navigate tool once (any URL), then mint the link again.", true
	}

	// Tool output passes through the shared secret redactor
	// (agentcore.governToolOutput). Its generic marker rule replaces 8+
	// characters after `token=` / `api_key=` / `secret=` and friends — so a
	// viewer URL carrying such a query parameter would reach the model as
	// [REDACTED], and assistant text is not redacted, meaning nothing
	// downstream could recover it. Detect that here and fail loudly rather than
	// hand back a link that silently became unusable. The fix if this ever
	// fires is a redactor-safe rendering, NOT a weaker redactor.
	//
	// The production redactor is agentcore's, which also carries registered
	// LITERALS (env secret values, plus runtime ones the remote-MCP resolver
	// notes). internal/agentcore imports this package, so it cannot be called
	// from here; approximate it instead — env literals the same way agentcore
	// does, plus this call's own credential, which is the literal most likely to
	// appear in a vendor URL and is not in the environment when it came from the
	// connector.
	guard := redact.NewRedactor(nil)
	guard.RegisterEnvLiterals(os.Environ())
	if strings.Contains(liveView, apiKey) {
		return "BROWSERBASE_URL_NOT_RELAYABLE: Browserbase returned a live-view URL containing the API key itself, which " +
			"this server redacts from tool output — so it cannot be passed on intact. Do NOT retry — tell the user to open " +
			"the session directly at https://www.browserbase.com/sessions.", true
	}
	if guard.Redact(liveView) != liveView {
		return "BROWSERBASE_URL_NOT_RELAYABLE: Browserbase returned a live-view URL whose shape collides with this " +
			"server's secret-redaction rules, so it cannot be passed on intact. Do NOT retry — tell the user to open the session " +
			"directly at https://www.browserbase.com/sessions, and report that the live-view URL format needs a fleet-side fix.", true
	}

	var b strings.Builder
	fmt.Fprintf(&b, "live_view_url: %s\n", liveView)
	fmt.Fprintf(&b, "session_id: %s\n", sessionID)
	fmt.Fprintf(&b, "pages: %d\n", len(decoded.Pages))
	b.WriteString("\nThis link grants control of the browser session to anyone who opens it, and stops working when the session ends. " +
		"Give it to the user, ask them to do what is needed and reply when done, then end your turn. " +
		"Do not call the connector's end tool until they confirm they are finished.")
	return b.String(), false
}

// browserbaseStatusError maps an HTTP status to an actionable message. The
// 401/403/404 case is the one worth spelling out: because the connector and this
// tool authenticate with DIFFERENT credentials, a live session can be invisible
// here simply because the two keys belong to different Browserbase projects —
// which otherwise looks identical to "the session ended".
func browserbaseStatusError(status int, sessionID string) (string, bool) {
	switch status {
	case http.StatusOK:
		return "", false
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return fmt.Sprintf("BROWSERBASE_SESSION_NOT_VISIBLE: Browserbase has no session %q under the API key this server holds (HTTP %d). "+
			"Either the session already ended, or this server's BROWSERBASE_API_KEY belongs to a different Browserbase project than the "+
			"connector's key. Do NOT retry — tell the user to check that both keys come from the same Browserbase project, and offer the "+
			"dashboard fallback at https://www.browserbase.com/sessions.", sessionID, status), true
	case http.StatusTooManyRequests:
		return "BROWSERBASE_UNAVAILABLE: Browserbase is rate-limiting this server (HTTP 429). Wait before trying again, and do not loop.", true
	default:
		return fmt.Sprintf("BROWSERBASE_UNAVAILABLE: the Browserbase API returned HTTP %d. Do NOT retry more than once — report it and "+
			"offer the dashboard fallback at https://www.browserbase.com/sessions.", status), true
	}
}
