---
name: connector-builder
description: "Build, configure, test, and save custom integrations and connectors right inside Fleet — automatically choosing the best tier (Official MCP server, Custom REST/GraphQL/XHR API connector, or Headless Chromium Playwright browser automation with Co-Piloted Live Browser View for logins/CAPTCHAs/2FA). Use whenever a user, Solutions Engineer, or admin asks to connect a website, portal, or service (create a connection, connect a website/API, integrate a service, or /connector-builder)."
---

# Fleet Connector Builder Studio

When a user, Solutions Engineer, or administrator asks to integrate a website, portal, or API into Fleet, your goal is to deliver a **working, verified connector** that appears in **Settings → Connections** (`/settings/connections`) and is immediately active in the current chat (and optionally shared Fleet-wide with everyone on the box).

You have two purpose-built native tools for this workflow:
- **`connector_studio`** — search Fleet's MCP directory, probe HTTP APIs, inspect websites with headless Chromium + Playwright (capturing DOM selectors and hidden XHR/JSON APIs), launch Co-Piloted Live Browser View sessions for human-assisted login/CAPTCHA/2FA, create custom `http_tools` or `playwright` connectors, and test custom tools end to end.
- **`show_ui`** — collect credentials (API keys, usernames, passwords) and sharing preferences (`personal` vs `everyone`) through an interactive form card using `format: "password"` so secrets are vaulted host-side (`$secret:<field_id>`) and never appear in plaintext in the transcript or prompt history.

---

## The 3-Tier Integration Hierarchy (Always Prefer Higher Tiers)

Always evaluate and choose the highest tier that works for the target service:

1. **Tier 1 — Official / Catalog MCP Server (`connect_mcp`)** *(Best)*
   - Always start by calling `connector_studio(action="search_catalog", query="<vendor or domain>")`.
   - If an official or curated MCP server exists in `catalog_matches` (or `bundled_matches`), use it! Official MCP servers are vendor-maintained and support OAuth or API keys out of the box.

2. **Tier 2 — Custom HTTP / API Connector (`transport="http_tools"`)** *(Preferred Custom Path)*
   - If there is no official MCP server, check whether the service has a REST, GraphQL, or JSON API — either documented by the vendor OR discovered by inspecting the web app's background XHR/fetch calls via `connector_studio(action="probe_browser", url="...", capture_network=true)`.
   - Whenever a clean JSON/REST/GraphQL endpoint exists, build a **`transport="http_tools"`** custom connector. Direct HTTP tools are fast, deterministic, and resilient to UI layout changes.

3. **Tier 3 — Headless Chromium + Playwright Browser Connector (`transport="playwright"`)** *(Universal Fallback)*
   - When a legacy portal or website has no usable API and requires real browser navigation, form submission, or DOM table/text extraction, build a **`transport="playwright"`** custom connector backed by Fleet's built-in headless Chromium runner (`playwrightrunner`).
   - If the login flow has a CAPTCHA, 2FA/MFA prompt, or SSO wall, use **Co-Piloted Live Browser View** (`action="start_copilot_session"`) so the user can log in interactively in a live browser tab and hand the authenticated session state (`storage_state` cookies + `localStorage`) straight back to you.

---

## Step-by-Step Workflow

### Step 1 — Search the Catalog & Recon the Target

1. Call `connector_studio(action="search_catalog", query="<service name or domain>")` first.
2. If the user gave you a URL or service name and you need credentials or scope preferences before probing or connecting, render a single, polished **`show_ui`** intake card (see Step 2).
3. If the target is a public URL or API doc page, you can immediately probe it with `connector_studio(action="probe_http", ...)` or `connector_studio(action="probe_browser", url="...", inspect_dom=true, capture_network=true)` to see how it works before asking the user for credentials.

### Step 2 — Collect Credentials & Scope Safely with `show_ui` (`format: "password"`)

**Never ask users to type raw passwords or API keys in plain chat messages.** Always collect secrets and scope using `show_ui`:

- Set `"format": "password"` on every `text_input` that collects a password, API key, token, or client secret.
- Fleet's UI masks `format: "password"` fields in the form (`••••`), excludes them from browser `localStorage` drafts, and vaults the submitted value on the host as `"$secret:<field_id>"` before it ever reaches the database or LLM.
- Include a `choice` component (`"variant": "radio"`, id: `"share_scope"`) asking whether the connector should be:
  - `"personal"` — **Just for me** (only visible and usable by the current user)
  - `"everyone"` — **Fleet-wide (Everyone on this box)** (shared with `*` so all teammates and scheduled tasks can use it)

Example `show_ui` credential & scope intake card:

```json
{
  "title": "Configure Acme Portal Connection",
  "components": [
    {
      "type": "section",
      "title": "Connection Details",
      "description": "Passwords and API keys are encrypted host-side and stay sealed on this device.",
      "children": [
        {
          "type": "text_input",
          "id": "portal_url",
          "label": "Portal or API URL",
          "format": "url",
          "value": "https://portal.acme.com",
          "required": true
        },
        {
          "type": "text_input",
          "id": "username",
          "label": "Username / Email (if using login)",
          "placeholder": "you@company.com"
        },
        {
          "type": "text_input",
          "id": "password",
          "label": "Password or API Key",
          "format": "password",
          "placeholder": "Enter password or API token",
          "required": true
        },
        {
          "type": "choice",
          "variant": "radio",
          "id": "share_scope",
          "label": "Who should have access to this connector?",
          "value": "personal",
          "options": [
            { "value": "personal", "label": "Just for me (Personal connection)" },
            { "value": "everyone", "label": "Everyone on this Fleet box (Fleet-wide)" }
          ]
        }
      ]
    }
  ],
  "actions": [
    {
      "id": "connect",
      "label": "Connect & Test",
      "kind": "submit",
      "style": "primary"
    }
  ]
}
```

When the user submits the card, you will receive a message like:
`[UI submission] {"portal_url":"https://portal.acme.com","username":"alice@acme.com","password":"$secret:password","share_scope":"everyone"}`

Pass `"$secret:password"` directly in `secret_ref` or `password_ref` on `connector_studio` — the host resolves the real secret in memory and seals it in `remote_mcp_servers.api_key_enc`.

### Step 3 — Probe & Discover (HTTP or Headless Chromium + Co-Pilot Live View)

- **For REST / GraphQL APIs (`probe_http`)**:
  - Call `connector_studio(action="probe_http", url="...", method="GET", secret_ref="$secret:password", api_key_header="...")`.
  - Inspect the status code and JSON body preview to design accurate tool endpoints and schemas.

- **For Websites & Portals (`probe_browser`)**:
  - Call `connector_studio(action="probe_browser", url="...", username_ref="...", password_ref="$secret:password", inspect_dom=true, capture_network=true, steps=[...])`.
  - Check `network_calls` in the result! Many web portals fetch clean JSON from an internal `/api/...` or `/graphql` endpoint after login. If you spot a clean API call, prefer wrapping that API or extracting straight from it.
  - Use `{{USERNAME}}` and `{{PASSWORD}}` inside Playwright step `value` fields (e.g. `{"action": "fill", "selector": "input[type=email]", "value": "{{USERNAME}}"}`, `{"action": "fill", "selector": "input[type=password]", "value": "{{PASSWORD}}"}`, `{"action": "click", "selector": "button[type=submit]"}`).

- **When Login Hits a CAPTCHA, 2FA/MFA, or SSO Wall (`start_copilot_session`)**:
  - Do not guess or loop on a blocked login. Immediately call:
    `connector_studio(action="start_copilot_session", url="...", username_ref="...", password_ref="$secret:password", steps=[...])`
  - This launches a live headless Chromium session with real-time JPEG screencasting and interactive mouse/keyboard control, returning `session_id` and `live_view_url` (`/api/browser-sessions/<id>/view?token=...`).
  - Present the link clearly as `[Open Live Browser View](<live_view_url>)`, tell the user what to complete (e.g. *"Click the link to solve the CAPTCHA / enter your 2FA code, then click **Done — Return to Chat** and reply here"*), and **end your turn**.
  - When the user replies that they are done:
    1. Call `connector_studio(action="probe_browser", session_id="<session_id>", inspect_dom=true, capture_network=true)` to inspect the authenticated page and design the extraction steps.
    2. Pass `session_id="<session_id>"` to `connector_studio(action="create_custom_connector", ...)` — Fleet automatically exports the live session's authenticated cookies and `localStorage` (`storage_state`) and seals them into the connector so future tool calls stay logged in!

### Step 4 — Create & Save the Connector (`connect_mcp` or `create_custom_connector`)

- **Official MCP (`connect_mcp`)**:
  ```json
  {
    "action": "connect_mcp",
    "name": "hubspot",
    "url": "https://mcp.hubspot.com/mcp",
    "auth_mode": "oauth",
    "share_scope": "everyone"
  }
  ```
- **Custom HTTP API Connector (`create_custom_connector` with `transport="http_tools"`)**:
  - Define 1 to 6 high-value, well-named `snake_case` tools in `custom_tools`.
  - Use `{param}` placeholders in `url` or `body` matching properties in `input_schema`.
- **Custom Playwright Browser Connector (`create_custom_connector` with `transport="playwright"`)**:
  - Define declarative `steps` (`goto`, `fill`, `click`, `wait_for`, `press`, `select`, `extract`, `sleep`).
  - Include at least one `extract` step (`mode: "text"`, `"table_json"`, `"html"`, or `"attribute"`) so the tool returns structured data from the page.
  - Pass `session_id` if the user authenticated in a Co-Pilot Live Browser session so their `storage_state` cookies are sealed into the connector.

### Step 5 — Verify End-to-End with `test_custom_tool`

Always test at least one read-only tool on every custom connector you create before finishing:
```json
{
  "action": "test_custom_tool",
  "name": "acme_portal",
  "tool_name": "get_dashboard_summary",
  "tool_args": {}
}
```
If `test_custom_tool` reports an error (e.g., a selector timed out or an endpoint path was wrong), fix the tool definition and call `create_custom_connector` again (calling `create_custom_connector` with the same `name` updates the custom connector in place) and re-test until it succeeds!

### Step 6 — Delight the User with a Clear Summary

Once verified, present a concise summary showing:
- **Connector name & type**: e.g. `acme_portal` (`Custom API` or `Browser Automation` or `Official MCP`)
- **Access scope**: Personal (`Just for you`) or Fleet-wide (`Shared with everyone on this box`)
- **Registered tools**: List each `mcp_<name>_<tool>` and what it does
- **Live verification result**: Briefly show the real data returned by `test_custom_tool`
- **Where to manage it**: Link to [Settings → Connections](/settings/connections)
