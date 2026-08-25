# ADR-0050: Multiple logins per hosted MCP connection ("seats")

- **Status:** Accepted
- **Date:** 2026-08-25
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0009](0009-per-user-remote-mcp-oauth.md) (one connection per
  user per server name → one per `(user, name, account)`)

## Context

ADR-0009 gave every user their own OAuth-connected hosted MCP servers, keyed
`(user_email, name)`: one token per user per catalog entry. Bundled connectors
meanwhile grew a seat model — `<VAR>_<ACCOUNT>` env overlays, a per-user
default seat, per-task `{server, account}` pins. A user who needed a work and
a personal GitHub through the official hosted server had no clean way to hold
both (#988), and an approval staged against a hosted connection could not
record which login it ran as.

## Decision

1. **A hosted connection name may carry several seats**, each a row with a
   public `account` label, its own sealed credential, and its own share
   grants. Exactly one seat per `(user, name)` is `is_default`, enforced by a
   partial unique index. The first seat under a name becomes the default;
   deleting the default promotes another seat.
2. **A run mounts exactly one seat per name**, registered under the bundle
   formula `RegisteredMCPName(name, account)`. Resolution: pinned label, else
   the default seat. **A pin that cannot be honored is skipped, never
   substituted** — a run must not transact as a different account than the
   one asked for.
3. **The AEAD AAD stays `(purpose, owner email, canonical url)`** and does not
   include the label or the row id. Labels must be renamable without
   re-sealing, and a transplant between two seats of the same user at the same
   vendor requires DB write access, at which point the attacker can already
   change `is_default` or the share grants. Cross-user and cross-vendor
   transplants remain rejected as before.
4. **The broker boundary carries labels, never credentials.**
   `RemoteScopeSpec.accounts`/`exact` are public; the credential-owning child
   resolves seats from its own store and the parent only maps returned
   registration names back to `{name, account}` for attribution.
5. **Approval re-execution is `Exact`.** A staged card records the seat that
   was mounted; approve/execute reopens that literal seat even if the default
   has since moved. (Bundled seats got this in #167 residual 2; this is the
   hosted half.)

## Consequences

- Sharing stays per seat: sharing "work" does not expose "personal".
- Tool names follow the mounted seat (`mcp_<name>_<account>_<tool>` for a
  labeled seat), consistent with bundled named accounts.
- A conversation may override the seat per connector
  (`conversations.mcp_accounts`), for hosted and bundled connectors alike;
  tasks pin via `mcp_selection` as they always did, and hosted names in a
  selection are routed to the overlay rather than the bundle binder.
- Deployments with no hosted connections see no behaviour change: the
  migration flags every existing row as its name's default, and a runner
  without remote-MCP capability keeps every selection name on the bundle side.
