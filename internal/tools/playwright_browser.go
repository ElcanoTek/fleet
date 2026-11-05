package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/playwrightrunner"
)

// PlaywrightBrowserToolName is the canonical name of the built-in local
// Playwright + headless Chromium browser automation tool (with Co-Pilot Live View).
const PlaywrightBrowserToolName = "playwright_browser"

// PlaywrightBrowserParams defines the schema-generating input parameters for playwright_browser.
type PlaywrightBrowserParams struct {
	Action         string                  `json:"action,omitempty" description:"'run' (default: headless Chromium run with optional steps, DOM inspection, and network capture), 'start_session' (start an interactive Co-Piloted Live Browser Session and return a live_view_url for the user to solve logins/CAPTCHAs/2FA), 'steps' (run steps in an existing session_id), 'snapshot' (inspect current page of session_id after the user interacts), or 'close_session' (close session_id)."`
	URL            string                  `json:"url,omitempty" description:"Target https:// URL to open (required for 'run' and 'start_session')."`
	SessionID      string                  `json:"session_id,omitempty" description:"Co-Pilot browser session_id from 'start_session' (required for 'steps', 'snapshot', and 'close_session')."`
	Steps          []playwrightrunner.Step `json:"steps,omitempty" description:"Ordered declarative Playwright actions: each step has action ('goto'|'fill'|'click'|'wait_for'|'press'|'select'|'extract'|'sleep'), selector (CSS or Playwright text selector), value (use '{{USERNAME}}' / '{{PASSWORD}}' / '{{SECRET}}' for credentials), mode ('text'|'html'|'table_json'|'attribute' for extract), attribute, timeout_ms, optional."`
	UsernameRef    string                  `json:"username_ref,omitempty" description:"Optional username or '$secret:<field_id>' reference from a show_ui submission."`
	PasswordRef    string                  `json:"password_ref,omitempty" description:"Optional '$secret:<field_id>' reference from a show_ui password field submission (resolved host-side, never exposed in plaintext)."`
	InspectDOM     bool                    `json:"inspect_dom,omitempty" description:"When true, returns interactive DOM elements (inputs, buttons, forms, tables) and their selectors."`
	CaptureNetwork bool                    `json:"capture_network,omitempty" description:"When true, captures XHR/fetch/JSON/GraphQL API calls made by the page so you can discover hidden JSON APIs."`
}

// PlaywrightBrowserContext carries per-turn user and secret resolution callbacks
// for playwright_browser.
type PlaywrightBrowserContext struct {
	UserEmail     string
	PublicBaseURL string
	ResolveSecret func(ref string) string
}

type playwrightBrowserCtxKey struct{}

// WithPlaywrightBrowserContext attaches per-turn user and secret-resolution
// metadata to ctx for playwright_browser.
func WithPlaywrightBrowserContext(ctx context.Context, pctx PlaywrightBrowserContext) context.Context {
	return context.WithValue(ctx, playwrightBrowserCtxKey{}, pctx)
}

func playwrightBrowserContextFrom(ctx context.Context) PlaywrightBrowserContext {
	if ctx == nil {
		return PlaywrightBrowserContext{}
	}
	v, _ := ctx.Value(playwrightBrowserCtxKey{}).(PlaywrightBrowserContext)
	return v
}

const playwrightBrowserDescription = `Drive a built-in headless Chromium browser via Playwright, inspect rendered DOM and XHR/JSON network traffic, or open a Co-Piloted Live Browser View link so the user can solve a login, CAPTCHA, or 2FA in real time — with no Browserbase account or external API key required.

Actions:
- 'run' (default): Navigate to 'url', run optional declarative 'steps' (fill, click, wait_for, press, select, extract), and return rendered page text, extracted data, interactive DOM selectors ('inspect_dom': true), and background XHR/fetch/JSON network calls ('capture_network': true).
- 'start_session': Launch a persistent Co-Piloted Live Browser Session at 'url' and return 'session_id' + 'live_view_url'. Share 'live_view_url' as a markdown link with the user whenever a page needs human help (CAPTCHA, 2FA/MFA, SSO login, or complex consent wall), tell them what to do in the live browser tab, and END YOUR TURN so they can reply when done.
- 'snapshot': Inspect the current page of an active 'session_id' after the user finishes logging in or solving a CAPTCHA in the Co-Pilot Live View.
- 'steps': Execute additional Playwright 'steps' inside an active 'session_id' (preserving the user's logged-in cookies and session state).
- 'close_session': Close 'session_id' when the task is complete.

Credentials:
- Collect passwords/tokens via show_ui with format='password' (which vaults the value host-side as '$secret:<field_id>') and pass that reference in 'password_ref', using '{{USERNAME}}' and '{{PASSWORD}}' inside step values.`

// NewPlaywrightBrowserTool constructs the built-in playwright_browser tool.
func NewPlaywrightBrowserTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		PlaywrightBrowserToolName,
		playwrightBrowserDescription,
		func(ctx context.Context, params PlaywrightBrowserParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			res, err := executePlaywrightBrowserTool(ctx, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			out, err := json.MarshalIndent(res, "", "  ")
			if err != nil {
				return fantasy.NewTextErrorResponse("playwright_browser: failed to encode result"), nil //nolint:nilerr // Tool errors are returned in TextErrorResponse.
			}
			return fantasy.NewTextResponse(string(out)), nil
		},
	)
}

func executePlaywrightBrowserTool(ctx context.Context, params PlaywrightBrowserParams) (any, error) {
	action := strings.ToLower(strings.TrimSpace(params.Action))
	if action == "" {
		action = "run"
	}
	pctx := playwrightBrowserContextFrom(ctx)
	resolve := func(val string) string {
		val = strings.TrimSpace(val)
		if val == "" {
			return ""
		}
		if pctx.ResolveSecret != nil {
			if resolved := pctx.ResolveSecret(val); resolved != "" {
				return resolved
			}
		}
		return val
	}
	creds := playwrightrunner.Credentials{
		Username: resolve(params.UsernameRef),
		Password: resolve(params.PasswordRef),
		Secret:   resolve(params.PasswordRef),
	}

	switch action {
	case "run":
		cleanURL, err := playwrightrunner.ValidateTargetURL(params.URL, false)
		if err != nil {
			return nil, err
		}
		return playwrightrunner.Run(ctx, playwrightrunner.RunRequest{
			URL:            cleanURL,
			Steps:          params.Steps,
			Credentials:    creds,
			InspectDOM:     params.InspectDOM,
			CaptureNetwork: params.CaptureNetwork,
		})

	case "start_session":
		if strings.TrimSpace(pctx.UserEmail) == "" {
			return nil, errors.New("start_session is only available in interactive user conversations")
		}
		cleanURL, err := playwrightrunner.ValidateTargetURL(params.URL, false)
		if err != nil {
			return nil, err
		}
		info, err := playwrightrunner.DefaultSessionManager().StartSession(ctx, playwrightrunner.SessionStartRequest{
			OwnerEmail:  pctx.UserEmail,
			URL:         cleanURL,
			Steps:       params.Steps,
			Credentials: creds,
			InspectDOM:  params.InspectDOM,
		})
		if err != nil {
			return nil, err
		}
		viewPath := fmt.Sprintf("/api/browser-sessions/%s/view?token=%s", info.ID, info.ViewToken)
		liveURL := viewPath
		if base := strings.TrimRight(strings.TrimSpace(pctx.PublicBaseURL), "/"); base != "" {
			liveURL = base + viewPath
		}
		return map[string]any{
			"status":        "session_started",
			"session_id":    info.ID,
			"live_view_url": liveURL,
			"current_url":   info.CurrentURL,
			"title":         info.Title,
			"expires_at":    info.ExpiresAt,
			"result":        info.Result,
			"instructions":  "Share live_view_url with the user as a clickable markdown link so they can interact with the live browser (solve CAPTCHA, 2FA, or sign in), and END YOUR TURN. When they reply, call playwright_browser with action='snapshot' or action='steps' and this session_id, or pass this session_id to connector_studio create_custom_connector to save the authenticated session cookies!",
		}, nil

	case "snapshot":
		sess, err := playwrightrunner.DefaultSessionManager().GetSession(params.SessionID, pctx.UserEmail, "")
		if err != nil {
			return nil, err
		}
		res, err := sess.Snapshot(ctx, params.InspectDOM, false)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"session_id": params.SessionID,
			"result":     res,
		}, nil

	case "steps":
		sess, err := playwrightrunner.DefaultSessionManager().GetSession(params.SessionID, pctx.UserEmail, "")
		if err != nil {
			return nil, err
		}
		res, err := sess.RunSteps(ctx, params.Steps, params.InspectDOM, false)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"session_id": params.SessionID,
			"result":     res,
		}, nil

	case "close_session", "close", "end":
		if err := playwrightrunner.DefaultSessionManager().CloseSession(params.SessionID, pctx.UserEmail, ""); err != nil {
			return nil, err
		}
		return map[string]any{
			"status":     "closed",
			"session_id": params.SessionID,
		}, nil

	default:
		return nil, fmt.Errorf("unsupported playwright_browser action %q (expected 'run', 'start_session', 'snapshot', 'steps', or 'close_session')", params.Action)
	}
}
