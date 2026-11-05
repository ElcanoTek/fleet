# Connector Builder (`connector-builder` & `connector_studio`)

Design note for the interactive **Connector Builder** skill (`/connector-builder`), the native `connector_studio` and `playwright_browser` tools, custom `http_tools` and `playwright` connectors in `remote_mcp_servers`, host-side `show_ui` password secret vaulting, and the **Co-Piloted Live Browser View** (`/api/browser-sessions/{id}/view`).

---

## 1. What Shipped

### 1.1 Tiered Connector Strategy & Built-In Skill (`internal/clientconfig/builtin_skills/connector-builder/SKILL.md`)

Fleet ships a built-in `/connector-builder` skill and a **"Create your own connection"** shortcut button in **Settings → Connections** (`/chat?prompt=%2Fconnector-builder+Help+me+create+a+custom+connection`).

When a user or solutions engineer asks to integrate a third-party service, the builder evaluates three tiers in priority order:

1. **Tier 1 — Official / Catalog MCP Server (`streamable_http` / `sse`)**:
   - Searches `RemoteMCPCatalog` and bundled MCP servers via `connector_studio(action="search_catalog")`.
   - Connects via OAuth 2.1 + PKCE or API key (`connector_studio(action="connect_mcp")`).
2. **Tier 2 — Custom API Wrapper Connector (`transport="http_tools"`)**:
   - Discovers or probes REST / GraphQL / JSON endpoints via `connector_studio(action="probe_http")` (read-only `GET`/`HEAD` only) or `connector_studio(action="probe_browser", capture_network=true)`.
   - Saves a declarative `http_tools` connector in `remote_mcp_servers` (`custom_tools` JSONB column, migration `025`) whose tools mount per-run as `mcp_<connector>_<tool>` through the standard MCP overlay and governance pipeline.
3. **Tier 3 — Local Playwright + Headless Chromium Browser Connector (`transport="playwright"`)**:
   - Inspects pages and tests declarative browser steps via `connector_studio(action="probe_browser")` and `playwright_browser`.
   - Supports **Co-Piloted Live Browser View** (`connector_studio(action="start_copilot_session")` / `playwright_browser(action="start_session")`) at `/api/browser-sessions/{id}/view` so the user can complete interactive login, CAPTCHA, 2FA, or SSO in a live remote-controlled headless Chromium session, then export and seal the authenticated `storage_state` (cookies + `localStorage`) into `remote_mcp_servers.api_key_enc`.

### 1.2 Host-Side `show_ui` Password Secret Vaulting (`internal/httpapi/connector_studio.go`)

- Before a `[UI submission] card=<id> action=<id>` message is queued, persisted to Postgres, or sent to the LLM, `vaultUISubmissionSecrets` resolves the submitted `card=<id>` against the conversation history, extracts every field declared with `format: "password"` on that card (plus defense-in-depth secret field IDs), stores the plaintext in an in-memory per-`(user, conversation)` vault (`uiSecretStash`, 2h TTL), registers the literal with the process-wide secret redactor (`agentcore.RegisterSecretLiterals` under a user-scoped key), and replaces the value in the submission JSON with `"$secret:<field_id>"`.
- In the web transcript (`web/src/app/chat/ui/genui/SubmissionBubble.tsx`), raw fallback JSON views parse fenced ```` ```json ```` blocks and mask password fields as `"••••••••"`.
- Host-side tool handlers (`connector_studio` and `playwright_browser`) resolve `$secret:<field_id>` references only after verifying:
  1. The target URL uses `https://` (`mcpoauth.ValidateServerURL` / `playwrightrunner.ValidateTargetURL`).
  2. The target host was explicitly confirmed by the calling user in the conversation (via their `show_ui` form submission or chat message) or matches the operator-curated `RemoteMCPCatalog` entry for the specifically selected catalog connector name.

### 1.3 Governance, SSRF, and Sandbox Invariants (`internal/agentcore`, `internal/playwrightrunner`, `internal/remotemcp`)

- **Mutating Browser Step Approval Gate**:
  - Native `playwright_browser` and `connector_studio` calls carrying mutating browser steps (`fill`, `click`, `press`, `select`) are classified as risky by `classifyRiskyBrowser` (normalizing all action aliases) and staged for explicit user approval via `checkBrowserSafety` in `InteractivePolicy.BeforeToolCall`.
  - Saved custom connector tools (`mcp_<connector>_<tool>`) mark non-`GET` HTTP tools and Playwright tools containing `fill`/`click`/`press`/`select` steps with `Tool.Critical = true` per-run (`o.SetRunCriticalTools`) so they route through the interactive approval card and scheduled `confirm_audit` gate.
- **SSRF & DNS Rebinding Protection**:
  - Every `probe_http` and `http_tools` request dials through `mcpoauth.SafeHTTPClient` / `netguard.DialContext`.
  - Every headless Chromium launch (`internal/playwrightrunner`) pre-flight resolves and validates target and `goto` step hosts via `netguard`, sets `--host-resolver-rules` to pin verified public IPs and block private/loopback/metadata ranges, and installs a Playwright route interceptor blocking non-HTTP(S) schemes and private IP literals.
  - URL placeholders in `playwright` tool templates (`{param}`) are URL-escaped (`url.PathEscape` / `url.QueryEscape` with `.`/`..` path-segment protection) before substitution.
  - Headless Chromium fails closed if the OS sandbox cannot initialize rather than falling back to `--no-sandbox`.

---

## 2. Scope & Deviations

- **Built-in local headless Chromium vs. external cloud browser**:
  - Instead of requiring an external Browserbase account for Tier 3 connectors and Co-Pilot login flows, Fleet runs headless Chromium locally on the host via `internal/playwrightrunner` (`scripts/bootstrap.sh` and `scripts/update.sh` converge `chromium-headless` and Python `playwright`). Catalog support for Browserbase remains optional for operators who explicitly configure it.
- **Declarative step DSL instead of arbitrary JS evaluation**:
  - Custom `playwright` connectors execute a bounded declarative step DSL (`goto`, `wait_for`, `fill`, `click`, `press`, `select`, `sleep`, `extract`) rather than arbitrary page JS evaluation (`evaluate` is rejected at validation and runtime).

---

## 3. Deferred Work

- **Automatic background session refresh for expiring cookie-only connectors**:
  - When a `playwright` connector relies on an exported `storage_state` session cookie without reusable `{{USERNAME}}`/`{{PASSWORD}}` login steps and the upstream site expires the cookie, the user re-runs a Co-Pilot session to refresh the sealed `storage_state`. Automated background re-login challenges are deferred.
- **Multi-tab / pop-up window streaming in Co-Pilot View**:
  - The Co-Pilot Live Browser View streams the primary page viewport and forwards click, key, scroll, and navigation events on that page; OAuth pop-ups that open in a separate browser window rather than redirecting in-page are not yet multiplexed into the single canvas view.
