# Hosted MCP connectors — operator runbook

How to connect a hosted (official, per-user OAuth) MCP server to fleet, what
each vendor needs before the Connect button works, how to tell a fleet
problem from a vendor problem, and what to do when a connection stops. Every
statement here was measured against the vendor in the #1006 OAuth pack; the
per-vendor results are in [`MCP-CATALOG-STATUS.md`](MCP-CATALOG-STATUS.md).
The design behind the mechanics is in [ADR-0009](adr/0009-per-user-remote-mcp-oauth.md)
(per-user OAuth), [ADR-0050](adr/0050-remote-mcp-seats.md) (seats),
[`REMOTE-MCP-MULTI-LOGIN.md`](REMOTE-MCP-MULTI-LOGIN.md) and
[`CONNECTION-SHARING.md`](CONNECTION-SHARING.md); the directory itself in
[`MCP-CATALOG.md`](MCP-CATALOG.md) and [`CONNECTOR-ONBOARDING.md`](CONNECTOR-ONBOARDING.md).

## Before the first connection

- **The callback URL is derived, not configured.** fleet advertises
  `FLEET_PUBLIC_BASE_URL` + `/api/oauth/mcp/callback` and logs it at boot
  (`remote MCP OAuth: ENABLED … redirect …`). `FLEET_PUBLIC_BASE_URL` must be
  the browser's origin for the **web tier**, not the chat-server port. Every
  vendor that asks for a redirect URI gets exactly that string.
- **Some vendors refuse plain-http callbacks.** Slack requires HTTPS. Microsoft
  Entra allows `http://` only for `localhost` (not `127.0.0.1`) and ignores the
  port there. Production deployments sit behind Caddy with TLS and never see
  this; a local rig needs a tunnel for Slack.
- **`FLEET_MCP_OAUTH_ENCRYPTION_KEY` is the connection.** Tokens, client
  secrets and registration tokens are sealed with it, bound to (owner, URL).
  Lose or rotate it and every hosted connection must be reconnected by its
  owner. Keep it where the database backups are.
- **Three shapes of Connect.** *One-click* — the vendor registers fleet as a
  client itself (Notion, Linear, Stripe, Grafana Cloud, Uptime Robot, Plaid,
  Cartesia, Globalping, …): the user clicks Connect and signs in. *Bring your
  own client* — the vendor has no self-registration: an admin creates an OAuth
  app at the vendor with fleet's callback URL and users paste the client ID
  and, where the directory says **required**, the secret (GitHub, Slack, the
  Google Workspace servers, Azure DevOps, Sage Intacct, …). *Tenant* — the
  URL carries the customer's org or host; the form asks for it, then one of
  the two flows above follows.
- **Why both a client secret and a login.** The secret identifies fleet, the
  application, to the vendor; the login identifies the user whose data the
  tools will act on. Vendors without self-registration need the first done by
  hand; the second is always per user.

## Per-vendor notes (measured)

- **GitHub** — OAuth App (not a GitHub App) with fleet's callback as the
  Authorization callback URL; the secret is mandatory — without it GitHub
  answers `incorrect_client_credentials` with HTTP 200. Tokens live 8 hours
  with a refresh token; `bad_refresh_token` means the user must reconnect.
  "Revoke all user tokens" on the app is noticed at the next mount (401 →
  *Reconnect needed*). No revocation endpoint: sign-out clears fleet's copy.
- **Google Workspace (Drive, Gmail, …)** — a Google Cloud OAuth client (Web
  application) with fleet's callback; secret required. Enable the Workspace
  MCP APIs on the project. fleet asks for `access_type=offline` and
  `prompt=consent`, without which Google issues a one-hour token and no
  refresh token. **The Workspace MCP servers are a Developer Preview**: a
  personal Gmail account connects but every tool call answers
  `The caller does not have permission`. Test with a Workspace account that is
  enrolled.
- **Slack** — create the app from a manifest or by hand; under *Features →
  Agents & AI Apps* turn on **Model Context Protocol** (without it the server
  answers `App is not enabled for Slack MCP server access`); redirect URL must
  be HTTPS; secret required. The user token has no expiry and no refresh
  token; no revocation endpoint. The vendor's metadata names its bare origin
  as the resource while serving MCP at `/mcp` — fleet keeps the typed URL.
- **Notion, Linear** — one click. Linear publishes a revocation endpoint, so
  sign-out revokes at the vendor; Notion does not.
- **Azure DevOps** — the org must be connected to a Microsoft Entra tenant
  (a standalone personal-account org is refused by Microsoft). In that
  tenant: confirm the *Azure DevOps MCP* enterprise application exists (or
  `az ad sp create --id 2a72489c-aab2-4b65-b93a-a91edccf33b8`), create an app
  registration on the **Web** platform with fleet's callback, add a client
  secret, add the *Azure DevOps MCP* delegated permission and **grant admin
  consent**. Sign in with a **work account native to the tenant**: a personal
  Microsoft account, even one made a tenant Member during Azure sign-up, loops
  at Microsoft's account picker on the `organizations` endpoint fleet is
  directed to. That user must have opened `dev.azure.com/<org>` once, or every
  tool call answers `Identity … has not been materialized`. Tokens live about
  70 minutes with a rotating refresh token; no revocation endpoint.
- **Stripe** — one click. Every API tool needs `stripe_context` (the account
  id) and `livemode`; the model gets them from `list_available_accounts_or_orgs`
  first. Calls made without them answer HTTP 422.
- **Grafana Cloud, Uptime Robot** — one click. Uptime Robot's server returns a
  client secret at registration although fleet asked to be a public client;
  fleet stores and uses it.
- **Plaid, Intercom** — no protected-resource metadata; connect worked on the
  2026-09-14 build. `main` currently refuses them at Add (their MCP path
  answers 401 under every sub-path, which the discovery code reads as a
  server failure). Known and deliberately not fixed at the time of writing.
- **Square, Smartlead** — SSE-only endpoints. fleet's hosted-connector
  transport is streamable HTTP; these cannot connect today.

## Running with connections

- **Chat.** A connection mounts only when it is enabled in the conversation's
  Tools picker. The web UI pre-enables the default set; an API client must
  send `enabled_optional` naming the connection, or nothing hosted mounts and
  the model reports "no MCP tools".
- **Scheduled tasks** mount every connected connection of the task's owner on
  its default seat, subject to the overlay ceiling. The overlay caps mounting
  at `maxOverlayServers = 8` connected servers per user (`maxOverlayServers` in
  `internal/agent/remote_mcp_overlay.go`), applied uniformly to chat,
  scheduled runs, and the broker. Selection order follows connection list order
  (`internal/remotemcp/resolver.go`'s `ConnectedServersForUser`: the owner's
  own connections newest-first, then the ones shared with them); servers past the
  8-server cap are skipped and logged. `mcp_selection` pins choose which seat
  account mounts for a connector, but pins do **not** bypass the 8-server cap
  or reorder servers ahead of it. A pin to a seat that is not connected skips
  that connector and tells the model; a pin to a server the owner never
  connected dead-letters the task without a model call.
- **Expired tokens refresh headlessly** in both chat and scheduled runs,
  under a row lock. Network errors and 5xx stay transient (the refresh path in
  `internal/remotemcp/service.go`): the connection is skipped for that run,
  remains connected, and is retried on the next call. Only terminal OAuth
  errors (e.g. `invalid_grant`) mark the connection *Reconnect needed*
  (`needs_reauth`). In either case, the run completes without the skipped
  connector.
- **More than 128 tools** switches the run to deferred mode: the model finds
  tools with `tool_search`, reads their schema with `tool_describe` (which
  lists the required arguments) and calls them with `tool_call`, which refuses
  a call missing a required argument and names it.
- **Seats and sharing.** One connection name can hold several logins; each is
  a seat with one owner-chosen default. Sharing is per seat, the grantee never
  sees the token, tool calls act as the owner at the vendor, and a revoked
  share takes effect on the grantee's next turn. Share only what you would
  hand over: a grantee gets the seat's full tool surface.

## When something fails

Read `fleet.log` first. The lines that matter:

- `remote-mcp: skipping server "<name>" for <user> — token unavailable` —
  refresh failed; the row is or will be *Reconnect needed*. Have the owner
  click Connect.
- `remote-mcp: skipping server "<name>" … — failed to connect: … HTTP 404` —
  the vendor rejected the tools handshake; usually a wrong URL (check the
  vendor's documented MCP path), not a credential problem, although the
  model is currently told the connector "needs re-authorization" either way.
- `mcpbroker: tool call failed (masked to the caller): … HTTP 422 …` — the
  vendor refused the tool arguments. The model sees only
  `credential-owner call failed`; the real text is on this line.
- `scheduled task <id>: wired N remote MCP server(s) for <user>` /
  `skipped remote MCP server(s) needing re-auth or a missing pinned seat` —
  what a scheduled run mounted and what it could not.
- `Fantasy tools registered: … N MCP tools DEFERRED` — the run is in deferred
  mode.

At **Add** time, the error toast names every metadata location fleet tried.
`fetch protected-resource metadata … 404` on every location means the vendor
publishes none; `authorization-server issuer mismatch` means the vendor's
document names another issuer and neither that issuer nor the vendor's host
vouched for its endpoints; `client secret required` means the vendor accepts
no public clients and the form needs the secret.

At **sign-in**, errors starting with `AADSTS` come from Microsoft:
`50011` a redirect URI that does not match the app registration, `65001`
admin consent not granted, `90100` a stale sign-in page (start again in a
private window, or complete the new user's first sign-in at
myaccount.microsoft.com first).

## Adding a vendor to the directory

The catalog rules are in [`MCP-CATALOG.md`](MCP-CATALOG.md). Before listing a
hosted server, run discovery against it (the Connect button does exactly
that) and record: does it publish protected-resource metadata, does its
authorization server allow public clients (`none` in
`token_endpoint_auth_methods_supported`), does it self-register, and does the
URL you list answer the MCP `initialize` POST with 401 rather than 404. A
URL that discovers but 404s tool calls is the most common catalog error.
