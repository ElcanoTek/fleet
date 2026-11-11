# Connector Builder (`connector-builder` & `connector_studio`)

Design note for the interactive **Connector Builder** skill (`/connector-builder`), the native `connector_studio` and `playwright_browser` tools, custom `http_tools` and `playwright` connectors in `remote_mcp_servers`, host-side `show_ui` password secret vaulting, and the **Co-Piloted Live Browser View** (`/api/browser-sessions/{id}/view`).

---

## 1. What Shipped

### 1.1 Tiered Connector Strategy & Built-In Skill (`internal/clientconfig/builtin_skills/connector-builder/SKILL.md`)

Fleet ships a built-in `/connector-builder` skill and a **"Build a connection in chat"** shortcut button in **Settings → Connections** (`/chat?prompt=%2Fconnector-builder+Help+me+create+a+custom+connection`).

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
- In `connector_studio(action="probe_browser")` and `playwright_browser`, a resolved credential reaches the page through the `{{USERNAME}}`, `{{PASSWORD}}` and `{{SECRET}}` placeholders in a step's value, which the runner substitutes as each step starts. A step that uses one runs only while the top-level page is on the origin the run or live session started on: the check is made when the step starts, so after a redirect or a navigation to another origin the step is refused. On a live session the credential is also resolved for the session's own origin, whatever `url` the call names, so a secret confirmed for one site cannot be bound to a session started on another. Both tools hand their steps over with the placeholders intact (`TestConnectorStudioLiveSessionHandsStepsOverWithPlaceholders`, `TestLiveSessionCredentialsResolveForTheSessionOrigin`, `TestConnectorStudioLiveSessionKeepsSecretStepsOnTheSessionOrigin`, `TestRunStepsSecretPrecheckFollowsGotoSteps`, `TestRunAbortsSecretFillAfterCrossOriginRedirect`). Saved `playwright` connectors apply their sealed credentials the same way, plus their `storage_state` cookies and an auth header (the configured one, or `Authorization: Bearer <secret>` by default) or query parameter; the runners attach those headers only to requests on the run's own origin.

### 1.3 Governance, SSRF, and Sandbox Invariants (`internal/agentcore`, `internal/playwrightrunner`, `internal/remotemcp`)

- **Mutating Browser Step Approval Gate**:
  - Native `playwright_browser` and `connector_studio` calls carrying mutating browser steps (`fill`, `click`, `press`, `select`) are classified as risky by `classifyRiskyBrowser` (normalizing all action aliases through `agentcore.ClassifyBrowserCall`) and staged for explicit user approval via `checkBrowserSafety` in `InteractivePolicy.BeforeToolCall`.
  - Their approval card (`internal/httpapi/approval_browser_summary.go`) describes the call in words: whether it opens a live session, runs steps in one, or runs a new headless browser; each step, with credential placeholders named and a credential given as a plain value (or typed into a password-like field) hidden; where each credential comes from; and, for steps in a live session, the origin it started on and the origin its page is on now (`playwrightrunner.PeekSession`, which does not count as activity), with a note when they differ. Under **Details** it keeps the call's own arguments (`raw_args`) as the tool will read them (decoded into its parameters, one row per step), with the same values hidden.
  - Saved custom connector tools (`mcp_<connector>_<tool>`) mark non-`GET` HTTP tools and Playwright tools containing `fill`/`click`/`press`/`select` steps with `Tool.Critical = true` per-run (`o.SetRunCriticalTools`) so they route through the interactive approval card and scheduled `confirm_audit` gate.
- **SSRF & DNS Rebinding Protection**:
  - Every `probe_http` and `http_tools` request dials through `mcpoauth.SafeHTTPClient` / `netguard.DialContext`.
  - Every headless Chromium launch (`internal/playwrightrunner`) sends all browser traffic through an ephemeral loopback proxy that dials with `netguard.DialContext`: it resolves each host at connect time, refuses private, loopback, link-local (including cloud-metadata) and other `netguard.IsBlockedIP` addresses, and dials the exact address it checked, so DNS rebinding cannot slip through. Chromium is launched with `--proxy-bypass-list=<-loopback>` so loopback addresses also go through that proxy, WebRTC is limited to proxied connections, and a Playwright route interceptor aborts every request it intercepts whose URL is not HTTP(S).
  - URL placeholders in `playwright` tool templates (`{param}`) are URL-escaped (`url.PathEscape` / `url.QueryEscape` with `.`/`..` path-segment protection) before substitution.
  - Headless Chromium runs with its own sandbox on: every launcher passes Playwright's `chromium_sandbox` / `chromiumSandbox` option (Playwright adds `--no-sandbox` when it is left at its default, `false`), and no launch argument turns the sandbox off. Where the sandbox cannot start (for example, a host without unprivileged user namespaces, a container whose seccomp profile or dropped capabilities refuse a call the sandbox makes, or fleet running as root), the launch fails with an error rather than falling back to `--no-sandbox`. `TestRunnersLaunchWithChromiumSandbox` pins the option in every launcher and runs in CI. `TestLiveSessionChromiumRendererIsSandboxed` checks a live session's real renderer for the sandbox's seccomp filter and a user or PID namespace of its own; it is Linux-only and needs Playwright and Chromium installed, so CI's Go jobs skip it.

---

## 2. Scope & Deviations

- **Built-in local headless Chromium vs. external cloud browser**:
  - Instead of requiring an external Browserbase account for Tier 3 connectors and Co-Pilot login flows, Fleet runs headless Chromium locally on the host via `internal/playwrightrunner` (`scripts/bootstrap.sh` and `scripts/update.sh` converge `chromium-headless` and Python `playwright`). Catalog support for Browserbase remains optional for operators who explicitly configure it.
- **Declarative step DSL instead of arbitrary JS evaluation**:
  - Custom `playwright` connectors execute a bounded declarative step DSL (`goto`, `wait_for`, `fill`, `click`, `press`, `select`, `sleep`, `extract`) rather than arbitrary page JS evaluation (`evaluate` is rejected at validation and runtime).
- **The browser tools are expected to fail in the Helm chart's pod**:
  - On the supported single-box install (Fedora/RHEL under `deploy/fleet.service`) the unit applies no seccomp filter, namespace restriction or capability limit, and the sandbox starts on a Fedora host. The chart's fleet pod runs as uid 1000 with `RuntimeDefault` seccomp, `drop: ["ALL"]` and `allowPrivilegeEscalation: false`. In a rootless podman container with those settings (podman's default seccomp profile, every capability dropped, `no-new-privileges`) the sandbox can create its user namespace, but its `chroot(2)` is refused: that profile allows `chroot` only to a container holding `CAP_SYS_CHROOT`. The sandboxed launch then exits at startup, while adding back just `CAP_SYS_CHROOT` lets it start. The chart hard-codes `drop: ["ALL"]` with nothing added back, so in a Kubernetes deployment expect `probe_browser`, `start_copilot_session`, `playwright_browser` and saved `playwright` connectors to return a launch error. This was checked with podman, not on a cluster, and the exact refused call may differ by container runtime (containerd's `RuntimeDefault` is not the same profile as podman's). Making the tools work there would mean a pod security context that lets the sandbox start; the chart does not offer one today.

---

## 3. Deferred Work

- **Automatic background session refresh for expiring cookie-only connectors**:
  - When a `playwright` connector relies on an exported `storage_state` session cookie without reusable `{{USERNAME}}`/`{{PASSWORD}}` login steps and the upstream site expires the cookie, the user re-runs a Co-Pilot session to refresh the sealed `storage_state`. Automated background re-login challenges are deferred.
- **Multi-tab / pop-up window streaming in Co-Pilot View**:
  - The Co-Pilot Live Browser View streams the primary page viewport and forwards click, key, scroll, and navigation events on that page; OAuth pop-ups that open in a separate browser window rather than redirecting in-page are not yet multiplexed into the single canvas view.
