# Built-in Microsoft 365 OAuth and Graph MCP Surface (`internal/ms365mcp`)

Fleet ships a first-party, native Go Microsoft 365 OAuth 2.1 proxy and stateless
Streamable HTTP Microsoft Graph MCP server directly inside the `fleet` binary,
replacing the external `@softeria/ms-365-mcp-server` Node.js process (`ms365-mcp.service`).

## What shipped

| Piece | Where |
| --- | --- |
| Built-in Microsoft 365 OAuth + Graph MCP package | `internal/ms365mcp/` |
| Streaming in-process HTTP round-tripper dispatch | `internal/netguard/inprocess.go` (`mcpoauth` + `tools/download_url`) |
| Public router mounting (`MS365_MCP_CLIENT_ID` gate) | `cmd/fleet/router.go`, `cmd/fleet/web.go` |
| Configuration registry entries (`MS365_MCP_*`) | `internal/config/envreg/table.go`, `docs/CONFIGURATION.md` |

When `MS365_MCP_CLIENT_ID` is non-empty, `buildWebServer` mounts the Microsoft 365
OAuth and MCP routes on the public HTTP tree (`buildPublicRouter`):

- `GET /.well-known/oauth-authorization-server` — RFC 8414 authorization server metadata.
- `GET /.well-known/oauth-protected-resource` and `GET /.well-known/oauth-protected-resource/*` — RFC 9728 protected resource metadata.
- `POST /register` — RFC 7591 Dynamic Client Registration (`token_endpoint_auth_method: "none"`).
- `GET /authorize` — Validates `redirect_uri` against the configured allowlist, requires a client `S256` PKCE challenge, generates a server-side PKCE verifier (`clientCodeChallenge -> serverCodeVerifier`), and redirects to `https://login.microsoftonline.com/{tenant}/oauth2/v2.0/authorize`.
- `POST /token` — Verifies and consumes the client's PKCE `code_verifier`, exchanges authorization codes or refresh tokens with Microsoft Entra (`https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token`), preserves the authorized scope on code exchange, omits `scope` on refresh when the caller omits it, and falls back to public-client PKCE if Entra reports `AADSTS700025`.
- `POST /mcp` — Stateless Streamable HTTP JSON-RPC 2.0 MCP endpoint (`initialize`, `ping`, `tools/list`, `tools/call`, `prompts/list`, `resources/list`). Requires `Authorization: Bearer <graph_token>` and returns `401 Unauthorized` with `WWW-Authenticate` when missing, expired, or rejected by Microsoft Graph so `mcpoauth` refreshes automatically. `GET /mcp` returns `405 Method Not Allowed` with a JSON-RPC error payload.
- `GET /attachment` — Redeems a short-lived, single-use ticket (`?t=<ticket>`) minted by `get-mail-attachment-content` or `get-download-url` and streams the attachment or drive item bytes from Microsoft Graph with `Cache-Control: no-store` and `X-Content-Type-Options: nosniff`. Non-`GET` methods (including `HEAD`) are rejected with `405 Method Not Allowed` before ticket redemption so link probes cannot consume a single-use ticket.

## Security and architectural boundaries

1. **Credentials and capability URLs stay in the credential-owning broker (`fleet mcp-broker`)**:
   - Per-user Microsoft Graph OAuth tokens stay sealed in `chat.remote_mcp_servers` and are unsealed inside `fleet mcp-broker`, which registers its own in-process `ms365mcp.Handler` (`cmd/fleet/mcp_broker.go`) so `initialize`, `tools/list`, `tools/call`, and `refresh_token` exchanges run inside the credential-owning child without dialing loopback or sending Bearer tokens into the parent `fleet` process.
   - When `mcp-broker` is active, the parent web server sets `DisableAuthenticatedMCP: true` (returning `401 + WWW-Authenticate` on unauthenticated `POST /mcp` discovery probes while refusing bearer-authenticated `POST /mcp` calls) and proxies token-free `GET /attachment?t=<ticket>` requests over a private Unix domain socket (`cmd/fleet/mcp_broker_runtime.go`, `NewUnixSocketAttachmentProxy`) to `fleet mcp-broker`.
   - Pre-authenticated Microsoft Graph `@microsoft.graph.downloadUrl` capability URLs are stripped from tool results (`cleanODataMetadata`) and are never returned by `get-download-url`. All file and attachment downloads go through short-lived, single-use host-side `/attachment?t=<ticket>` URLs (`MS365_MCP_ATTACHMENT_URL_TTL_S`, default 300s, range 1..3600s).
2. **Mandatory S256 PKCE on authorization code flows**:
   - `/authorize` requires `code_challenge` with `code_challenge_method=S256`, and `/token` (`grant_type=authorization_code`) requires a `code_verifier` matching a pending challenge in `pkceStore`. Unbound authorization codes are rejected with `invalid_request` / `invalid_grant`.
3. **Streaming in-process dispatch (`internal/netguard/inprocess.go`)**:
   - On provisioned Fleet boxes, `scripts/lib/hostname.sh` maps the box's public FQDN to `127.0.0.1` in `/etc/hosts`. Outbound HTTP calls from Fleet to its own public origin would otherwise resolve to loopback and be refused by `netguard.IsBlockedIP`.
   - `netguard.WrapRoundTripper` intercepts requests whose URL matches `Config.MatchesInProcessURL` (exact origin match against `MS365_MCP_PUBLIC_URL` / `FLEET_PUBLIC_BASE_URL` or `MS365_MCP_ALIASES`, and path in `IsMS365Path`) and serves them in-process via an `io.Pipe()` streaming response writer. Large attachment downloads stream directly without buffering the response body in memory, while all other outbound URLs continue through `netguard`'s dial-time SSRF guard unchanged.
   - When a request arrives via a configured alias in `MS365_MCP_ALIASES` (for example, an existing `chat.remote_mcp_servers` row minted on `https://fleetdev.elcanotek.com` before production cutover), discovery and attachment URLs reflect that alias origin so `mcpoauth.Discover` issuer validation succeeds without database mutations.
4. **Dedicated streaming client for `/attachment`**:
   - Ordinary Graph JSON API calls use a 30-second client timeout (`Config.HTTPClient`).
   - `/attachment` upstream fetches use `Config.StreamHTTPClient`, which bounds TCP dial (10s), TLS handshake (10s), and response headers (30s) without a whole-exchange timeout so multi-minute attachment downloads are not cut off mid-stream.
5. **Approval gating on all mutating Microsoft 365 tools**:
   - Every mutating tool in `internal/ms365mcp/tools_catalog.json` (`readOnlyHint: false` — mail sends/replies/forwards/drafts/moves, calendar event creation/updates/deletes/RSVPs, Teams chat/channel sends and replies, and To Do task creation/updates) is marked `"critical": true` and published with `"critical": true` in `tools/list` so Fleet's MCP broker and per-run `runCriticalTools` gate require human approval in interactive chat and `confirm_audit` in scheduled runs.

## Tool surface (57 tools)

The embedded catalog (`internal/ms365mcp/tools_catalog.json`) exposes all 57 tools across six domains:

- **Mail (18)**: `list-mail-messages`, `list-mail-folders`, `list-mail-folder-messages`, `get-mail-message`, `read-mail-text`, `list-mail-attachments`, `get-mail-attachment-content`, `get-download-url`, `create-draft-email`, `create-reply-draft`, `create-reply-all-draft`, `create-forward-draft`, `send-mail`, `send-draft-message`, `reply-mail-message`, `reply-all-mail-message`, `forward-mail-message`, `move-mail-message`.
  - `list-mail-messages`, `list-mail-folder-messages`, and `get-mail-message` automatically expand `attachments($select=id,name,contentType,size,isInline)` (with automatic retry without `$expand` if combined with `$search` on a mailbox that rejects `$expand+$search`) and preserve `attachments` across `$select` projection so attachment IDs are visible in a single call.
  - `read-mail-text` enforces at most 20 `messageIds` per call, truncates bodies on UTF-8 rune boundaries, enforces a 300,000-rune aggregate output cap, unwraps SafeLinks/Proofpoint/URLDefense/Google redirects, strips tracking query parameters, and propagates HTTP 401 (`errGraphUnauthorized`) so expired tokens trigger OAuth refresh.
- **Calendar (10)**: `list-calendars`, `list-calendar-events`, `get-calendar-event`, `get-calendar-view`, `create-calendar-event`, `update-calendar-event`, `delete-calendar-event`, `accept-calendar-event`, `decline-calendar-event`, `tentatively-accept-calendar-event`.
- **Teams & Chats (19)**: `list-chats`, `get-chat`, `list-chat-messages`, `get-chat-message`, `list-chat-members`, `create-chat`, `send-chat-message`, `reply-to-chat-message`, `list-joined-teams`, `get-team`, `list-team-channels`, `get-team-channel`, `list-team-members`, `list-channel-messages`, `get-channel-message`, `list-channel-message-replies`, `send-channel-message`, `reply-to-channel-message`, `parse-teams-url`.
- **OneDrive & Search (4)**: `list-folder-files`, `get-drive-item`, `search-onedrive-files`, `search-query`.
- **To Do Tasks (4)**: `list-todo-task-lists`, `list-todo-tasks`, `create-todo-task`, `update-todo-task`.
- **Contacts (2)**: `list-outlook-contacts`, `get-outlook-contact`.

## Scope and deviations

- **In-memory ticket and PKCE stores**: Both `pkceStore` (15-minute TTL, 4,096-entry total cap, 64-entry per-client-IP cap) and `AttachmentTicketStore` (`MS365_MCP_ATTACHMENT_URL_TTL_S` default 300s, 256-entry cap) are held in process memory inside `fleet mcp-broker` (with `/authorize`, `/token`, and `/attachment` proxied from the parent web server over a private Unix domain socket when the broker is active).
- **Disabled by default**: When `MS365_MCP_CLIENT_ID` is unset (or when `<FLEET_CLIENT_CONFIG_DIR>/ms365_tools.json` exists and fails to load), neither the public routes nor the `netguard` in-process hook are registered.
