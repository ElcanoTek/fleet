# Sharing a remote MCP connection

A follow-up to per-user remote MCP OAuth (#443, docs/adr/0009): the owner of a
connected hosted MCP server can share it with **named users** or with
**everyone on this box**. Grantees' chat turns and scheduled tasks mount the
server's tools exactly like the owner's runs do.

## Trust model

- **The credential never moves.** OAuth tokens stay AEAD-sealed to the
  *(owner email, url)* AAD in the owner's row (`internal/secretbox`); a
  grantee's run resolves the server through
  `store.GetRemoteMCPServerForUse`, which returns the owner-attributed row, so
  the host-side refresh/mint path is byte-identical to the owner's own runs.
  The grantee never sees the token, in the UI or the API.
- **The vendor sees the owner.** Tool calls made through a shared connection
  authenticate as the owner at the vendor. The share panel says this plainly,
  and the overlay logs `run for <user> uses shared server <name> owned by
  <owner>` for attribution.
- **Revocation is immediate.** Grants resolve fresh per run
  (`remotemcp.Service.ConnectedServersForUser`); deleting a grant — or the
  server, via `ON DELETE CASCADE` — takes effect on the next turn.
- **Owner-only management.** Share/unshare/list-grants require the caller to
  own the server (enforced in the store, surfaced as 404 otherwise). Sharing
  with yourself and non-email grantees are rejected
  (`store.ErrRemoteMCPShareInvalid`).

## Mechanics

- Migration `031_remote_mcp_shares.sql`: `remote_mcp_shares(server_id,
  grantee, created_at)` with `grantee` a user email or `*`
  (`store.GranteeEveryone`).
- Resolution: a user's runnable set = own connected servers + connected
  servers shared with them. On a registration-name collision the user's own
  server wins and the shadowed shared server is skipped with a log line (the
  name is the broker routing key — two same-named servers cannot coexist in
  one run). The per-run overlay cap (`maxOverlayServers`) applies to the
  merged set.
- API: `GET /remote-mcp-servers` now returns `shares` (grants on your servers,
  keyed by id) and `shared_with_me` (owner-attributed rows).
  `POST /remote-mcp-servers/{id}/shares {"grantee": "a@b.com" | "*"}` grants;
  `DELETE /remote-mcp-servers/{id}/shares/{grantee}` revokes.
- UI: Settings → Connections — a Share control per owned server (grantee
  chips, add-by-email, "Share with everyone") and a read-only "Shared with
  you" section naming each owner.

## Honest scope

- A grantee gets the connection's **full tool surface** — there is no per-tool
  or read-only narrowing on a grant. Share accordingly.
- Grants name emails directly; a grant to a not-yet-provisioned email simply
  sits dormant until such a user exists. There are no groups.
- The chat Tools picker's per-conversation opt-in applies to shared servers
  the same as own servers; scheduled runs wire all connected (own + shared)
  servers subject to the task's MCP selection.
- Grants are **per seat** (#988): a connection name may hold several logins,
  and sharing the "work" seat never exposes "personal". A grantee sees the
  owner's default among the seats shared with them and has no per-grantee
  default preference; a shared seat never becomes the default under a name
  the grantee owns seats for. Details in `docs/REMOTE-MCP-MULTI-LOGIN.md`.
