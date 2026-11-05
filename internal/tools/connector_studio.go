package tools

import (
	"context"
	"encoding/json"
	"strings"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/playwrightrunner"
)

// ConnectorStudioToolName is the canonical name of the interactive tool that
// helps users, Solutions Engineers, and admins discover, probe, build, test,
// and save custom connectors right inside Fleet.
const ConnectorStudioToolName = "connector_studio"

// Supported actions on connector_studio.
const (
	ConnectorStudioActionSearchCatalog       = "search_catalog"
	ConnectorStudioActionProbeHTTP           = "probe_http"
	ConnectorStudioActionProbeBrowser        = "probe_browser"
	ConnectorStudioActionStartCopilotSession = "start_copilot_session"
	ConnectorStudioActionConnectMCP          = "connect_mcp"
	ConnectorStudioActionCreateCustom        = "create_custom_connector"
	ConnectorStudioActionTestCustomTool      = "test_custom_tool"
)

// ConnectorStudioToolSpec defines a single declarative tool inside a custom
// HTTP ("http_tools") or Playwright ("playwright") connector.
type ConnectorStudioToolSpec struct {
	Name        string                  `json:"name" description:"Snake_case tool name (e.g. 'list_campaigns', 'get_daily_metrics', 'export_report')."`
	Description string                  `json:"description" description:"Clear 1-2 sentence description of what the tool returns and when the model should call it."`
	Method      string                  `json:"method,omitempty" description:"For 'http_tools': HTTP verb ('GET', 'POST', 'PUT', 'PATCH', 'DELETE'). Defaults to 'GET'."`
	URL         string                  `json:"url" description:"Target https:// URL or path (may contain {param} placeholders matching properties in input_schema)."`
	Headers     map[string]string       `json:"headers,omitempty" description:"Optional static non-secret headers (e.g. {'Accept': 'application/json'}). Never put secrets here; secrets are injected automatically from the connector's sealed credential."`
	Body        string                  `json:"body,omitempty" description:"Optional request body template for POST/PUT/PATCH (may contain {param} placeholders)."`
	InputSchema map[string]any          `json:"input_schema,omitempty" description:"JSON Schema object describing the tool's parameters (e.g. {'type':'object','properties':{'query':{'type':'string'}}})"`
	Steps       []playwrightrunner.Step `json:"steps,omitempty" description:"For 'playwright' connectors: ordered browser steps ('goto', 'fill', 'click', 'wait_for', 'press', 'select', 'extract', 'sleep'). Use '{{USERNAME}}' and '{{PASSWORD}}' in step values and '{param}' for tool arguments."`
}

// ConnectorStudioParams is the schema-generating input struct for connector_studio.
type ConnectorStudioParams struct {
	Action string `json:"action" description:"One of: 'search_catalog' (check Fleet's 75+ official MCP catalog and existing connections first), 'probe_http' (test a REST/GraphQL/OpenAPI endpoint host-side with SSRF guard and vaulted secret injection), 'probe_browser' (inspect a website or login flow with local headless Chromium + Playwright, capturing DOM selectors and hidden XHR/JSON APIs), 'start_copilot_session' (open a live Co-Piloted Chromium session and return a live_view_url so the user can solve a CAPTCHA/2FA/SSO login), 'connect_mcp' (register an official/remote MCP server), 'create_custom_connector' (create and mount a custom 'http_tools' or 'playwright' connector in Settings → Connections), or 'test_custom_tool' (verify a saved custom connector tool end-to-end)."`

	// search_catalog
	Query string `json:"query,omitempty" description:"'search_catalog': vendor name, domain, or keyword (e.g. 'hubspot', 'slack', 'pagerduty', 'acme.com')."`

	// shared connector identity & sharing
	Name       string `json:"name,omitempty" description:"Connector name in snake_case (e.g. 'acme_portal', 'stripe', 'internal_bi'). Tools will register as mcp_<name>_<tool>."`
	Account    string `json:"account,omitempty" description:"Optional seat label when adding a second account under the same connector name (e.g. 'work', 'personal')."`
	ShareScope string `json:"share_scope,omitempty" description:"'personal' (default: only for the current user) or 'everyone' (fleet-wide: shared with '*' Everyone on this box in Settings → Connections)."`

	// probe_http / probe_browser / connect_mcp / create_custom_connector
	URL          string            `json:"url,omitempty" description:"Target https:// URL (MCP endpoint for 'connect_mcp', base URL for 'create_custom_connector', or target URL for 'probe_http'/'probe_browser'/'start_copilot_session')."`
	Method       string            `json:"method,omitempty" description:"'probe_http': HTTP method (default 'GET')."`
	Headers      map[string]string `json:"headers,omitempty" description:"'probe_http': optional non-secret HTTP headers."`
	Body         string            `json:"body,omitempty" description:"'probe_http': optional request body."`
	AuthMode     string            `json:"auth_mode,omitempty" description:"'connect_mcp' or 'create_custom_connector': 'api_key', 'open', or 'oauth' (MCP only)."`
	APIKeyHeader string            `json:"api_key_header,omitempty" description:"Header name for api_key auth (e.g. 'X-API-Key' or 'Authorization'); empty defaults to 'Authorization: Bearer <key>'."`
	APIKeyPrefix string            `json:"api_key_prefix,omitempty" description:"Optional scheme prefix before the key under api_key_header (e.g. 'Token token=' or 'Basic ')."`
	APIKeyQuery  string            `json:"api_key_query,omitempty" description:"Optional query parameter name when the vendor authenticates via URL query param instead of a header."`

	// Host-side vaulted secret references (from show_ui format='password' submissions)
	SecretRef   string `json:"secret_ref,omitempty" description:"Vaulted secret reference '$secret:<field_id>' from a show_ui password submission (or raw API key if typed). Resolved host-side and sealed into remote_mcp_servers.api_key_enc."`
	UsernameRef string `json:"username_ref,omitempty" description:"Username or '$secret:<field_id>' reference for browser login probes and Playwright connectors."`
	PasswordRef string `json:"password_ref,omitempty" description:"Vaulted password reference '$secret:<field_id>' from a show_ui password submission for browser login probes and Playwright connectors."`

	// Browser probing / Co-Pilot session / Playwright custom connectors
	SessionID      string                    `json:"session_id,omitempty" description:"Co-Pilot live browser session_id from 'start_copilot_session' (or playwright_browser). Passing session_id to 'probe_browser' inspects that live session; passing it to 'create_custom_connector' seals its authenticated cookies/localStorage (storage_state) into the connector!"`
	Steps          []playwrightrunner.Step   `json:"steps,omitempty" description:"'probe_browser' / 'start_copilot_session': declarative Playwright steps to run."`
	InspectDOM     bool                      `json:"inspect_dom,omitempty" description:"'probe_browser': inspect interactive DOM elements and selectors."`
	CaptureNetwork bool                      `json:"capture_network,omitempty" description:"'probe_browser': capture XHR/fetch/JSON/GraphQL API calls made by the page."`
	Transport      string                    `json:"transport,omitempty" description:"'create_custom_connector': 'http_tools' (preferred whenever a REST/JSON/GraphQL API is available or discovered) or 'playwright' (headless Chromium browser automation fallback)."`
	CustomTools    []ConnectorStudioToolSpec `json:"custom_tools,omitempty" description:"'create_custom_connector': list of tools to register on this custom connector."`

	// test_custom_tool
	ToolName string         `json:"tool_name,omitempty" description:"'test_custom_tool': name of the custom tool to invoke (without the mcp_<connector>_ prefix)."`
	ToolArgs map[string]any `json:"tool_args,omitempty" description:"'test_custom_tool': arguments object to pass to the custom tool."`
}

// ConnectorStudioHandler executes connector_studio actions for the turn's user.
// Injected onto the turn context by httpapi.
type ConnectorStudioHandler func(ctx context.Context, params ConnectorStudioParams) (any, error)

type connectorStudioHandlerKey struct{}

// WithConnectorStudioHandler attaches a per-turn ConnectorStudioHandler to ctx.
func WithConnectorStudioHandler(ctx context.Context, h ConnectorStudioHandler) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, connectorStudioHandlerKey{}, h)
}

func connectorStudioHandlerFromContext(ctx context.Context) ConnectorStudioHandler {
	if ctx == nil {
		return nil
	}
	h, _ := ctx.Value(connectorStudioHandlerKey{}).(ConnectorStudioHandler)
	return h
}

const connectorStudioDescription = `Build, probe, test, and register integrations and custom connectors in Fleet — from official MCP servers (Tier 1), to custom REST/GraphQL HTTP API connectors (Tier 2), to local Playwright + headless Chromium browser connectors with Co-Piloted Live Browser View (Tier 3).

Always follow the 3-tier hierarchy:
1. Tier 1 — Official / Catalog MCP ('search_catalog' → 'connect_mcp'):
   Call 'search_catalog' first! If an official MCP server exists in Fleet's catalog (or at the vendor's /mcp endpoint), connect it via 'connect_mcp' (or give the user the OAuth button in Settings → Connections).
2. Tier 2 — Custom HTTP API Connector ('probe_http' → 'create_custom_connector' with transport='http_tools'):
   If there is no MCP server, check for a REST, GraphQL, or JSON API (or use 'probe_browser' with capture_network=true to discover the JSON/XHR endpoints the web app calls). Verify the endpoint with 'probe_http', then call 'create_custom_connector' with transport='http_tools' and test it with 'test_custom_tool'.
3. Tier 3 — Local Playwright + Chromium Browser Connector ('probe_browser' / 'start_copilot_session' → 'create_custom_connector' with transport='playwright'):
   When a site has no usable API and requires browser interaction, use 'probe_browser' (with inspect_dom=true and capture_network=true) to test login and extraction steps. If the site blocks login with a CAPTCHA, 2FA, or SSO wall, call 'start_copilot_session' to get a 'live_view_url', hand that link to the user so they can log in live in their browser, and END YOUR TURN. Once they reply, pass that 'session_id' into 'create_custom_connector' (transport='playwright') to seal the authenticated session cookies (storage_state) + credentials into the connector, and verify with 'test_custom_tool'!

Credential & Scope Rules:
- Always collect passwords, API keys, and tokens using 'show_ui' with format='password' (which vaults the secret host-side as '$secret:<field_id>' so it never enters chat history or logs) and ask in the same form whether the connector should be 'personal' (Just for me) or 'everyone' (Fleet-wide: shared with Everyone on this box).
- Every connector created by 'connector_studio' is saved in Settings → Connections, enabled for the current chat immediately, and works in Chat and Scheduled Tasks.`

// NewConnectorStudioTool constructs the interactive connector_studio tool.
func NewConnectorStudioTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ConnectorStudioToolName,
		connectorStudioDescription,
		func(ctx context.Context, params ConnectorStudioParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			action := normalizeConnectorStudioAction(params.Action)
			if action == "" {
				return fantasy.NewTextErrorResponse(
					"invalid action: must be one of 'search_catalog', 'probe_http', 'probe_browser', 'start_copilot_session', 'connect_mcp', 'create_custom_connector', or 'test_custom_tool'",
				), nil
			}
			params.Action = action
			h := connectorStudioHandlerFromContext(ctx)
			if h == nil {
				return fantasy.NewTextErrorResponse("connector_studio is only available in interactive chat conversations"), nil
			}
			res, err := h(ctx, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if out, mErr := json.MarshalIndent(res, "", "  "); mErr == nil {
				return fantasy.NewTextResponse(string(out)), nil
			}
			return fantasy.NewTextErrorResponse("connector_studio: could not encode the result"), nil
		},
	)
}

func normalizeConnectorStudioAction(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "search_catalog", "search", "catalog", "discover", "find":
		return ConnectorStudioActionSearchCatalog
	case "probe_http", "test_http", "http_probe":
		return ConnectorStudioActionProbeHTTP
	case "probe_browser", "browser_probe", "inspect_browser", "playwright_probe":
		return ConnectorStudioActionProbeBrowser
	case "start_copilot_session", "start_session", "copilot_session", "live_view", "live_browser", "open_live_browser":
		return ConnectorStudioActionStartCopilotSession
	case "connect_mcp", "add_mcp", "register_mcp":
		return ConnectorStudioActionConnectMCP
	case "create_custom_connector", "create_connector", "save_connector", "create_custom":
		return ConnectorStudioActionCreateCustom
	case "test_custom_tool", "test_tool", "verify_tool":
		return ConnectorStudioActionTestCustomTool
	default:
		return ""
	}
}
