# ADR-0009: Per-user remote MCP servers via OAuth

- **Status:** Accepted
- **Date:** 2026-06-30
- **Deciders:** fleet maintainers

## Context

Until now every MCP server was declared in the out-of-repo client-config bundle
(ADR-0006) and credentialed host-side from the env-file store: stdio servers get
`<VAR>_<ACCOUNT>` env overlays, HTTP servers get static header values
(ADR-0003). That covers operator-provisioned connectors with shared or
account-scoped secrets, but not the growing class of **hosted MCP servers that
require a per-user OAuth login** — where each user authorizes the agent against
*their own* account and the server mints short-lived, per-user tokens (MCP
authorization spec, revision 2025-06-18: OAuth 2.1 + PKCE, RFC 9728/8414
discovery, RFC 7591 dynamic client registration, RFC 8707 resource indicators).

These servers must be addable **from the GUI at runtime** (not the bundle, which
is a reproducible git artifact) and usable in **both** interactive chat and
headless scheduled tasks. That introduces three things fleet did not have: a
per-user, runtime server catalog; an OAuth client implementation; and short-lived
per-user tokens that need silent refresh — including for a scheduled run with no
user present.

## Decision

We add per-user remote (hosted) MCP servers with an OAuth login flow, keyed by
the user's **email** (the universal identity across the chat store and the
elcano-auth orchestrator tier).

1. **Tokens are credentials and stay host-side (ADR-0003 holds).** Access and
   refresh tokens, the dynamic-registration client secret, and the RFC 7592
   registration access token live only in the fleet process and the chat
   Postgres store, **encrypted at rest** (AES-256-GCM, `internal/secretbox`)
   with the AEAD bound to `(purpose, email, canonical-server-URL)` so a stolen
   ciphertext cannot be replayed as another user's / server's. They never enter
   the sandbox, the model context, or logs. A bearer reaches a server only as the
   `Authorization` header the **host-side** MCP client writes onto the outbound
   request — the same boundary an HTTP MCP server's static header already used.
   The feature **fails closed**: with no `FLEET_MCP_OAUTH_ENCRYPTION_KEY` it is
   disabled and the endpoints say so, rather than storing plaintext.

2. **One canonical server identity.** `mcpoauth.CanonicalResourceURI` is the
   single normalizer used for the DB key, the encryption AAD, the OAuth `state`
   record, the RFC 8707 `resource` indicator, the `Authorization` attachment, and
   broker routing — so "URL-ish" drift (casing, default port, trailing slash)
   can never leak a bearer to the wrong resource.

3. **User-supplied URLs are dialed through an SSRF guard.** All discovery, token,
   and tool-call requests to user-typed servers go through an HTTP client
   (`mcpoauth.SafeHTTPClient`) whose dialer rejects private / loopback /
   link-local / metadata addresses at connect time (DNS-rebinding safe) and
   refuses redirects (no 30x can relay a bearer to a new origin).

4. **The governed loop is not forked (ADR-0001 holds).** A user's connected
   servers are wired into a run as a per-run *overlay* `mcp.Client` (built with a
   freshly-refreshed bearer), composed with the shared/bundle client via a
   `compositeBroker` that routes the overlay's server names to it. The shared
   long-lived client is **never** mutated with per-user secrets, so concurrent
   users cannot cross-contaminate. Chat and scheduled use the **same**
   `agent.ApplyMCPOverlay` seam and the **same** refresh path.

5. **Refresh is serialized and rotation-safe.** `store.EnsureFreshToken` holds a
   `SELECT … FOR UPDATE` row lock, double-checks expiry after acquiring it, and
   writes any rotated (OAuth 2.1 single-use) refresh token in the same
   transaction it commits — so two concurrent turns/tasks issue at most one
   network refresh and both observe the rotation. A dead refresh token marks the
   connection `needs_reauth` and the server is **skipped** (graceful
   degradation), never failing the whole run.

## Enforcement

- At-rest crypto + AAD binding: `internal/secretbox` (`TestOpenWrongAADFails`,
  `TestNilCipherFailsClosed`) and `internal/store/remote_mcps_test.go`
  (`TestRemoteMCPNoCipherFailsClosed`, cross-user/AAD isolation).
- SSRF blocklist + canonicalization: `internal/mcpoauth` (`TestIsBlockedIP`,
  `TestSafeHTTPClientBlocksLoopback`, `TestCanonicalResourceURI*`).
- OAuth flow correctness (PKCE, RFC 8707 `resource` on authorize/exchange/refresh
  with `invalid_target` fallback, `invalid_grant` → reauth, refresh rotation,
  single-use state, completing-user binding): `internal/mcpoauth/flow_test.go`,
  `internal/remotemcp/service_test.go`, `internal/store` refresh-lock tests.
- The overlay never mutates the shared client and routes by server identity:
  `internal/agent` overlay + `compositeBroker`; the existing agentcore gate tests
  still pass unchanged.

## Consequences

- Users can self-serve hosted MCP connectors that need OAuth, for chat and their
  scheduled tasks, without operator involvement per connector.
- The chat-server process now holds live per-user bearer tokens **during a run**
  (in the overlay client). This is consistent with ADR-0003 (host-side, never in
  the sandbox/model/logs) but is worth stating: the out-of-process broker owns
  the *bundle* catalog; per-user overlay servers run in-process because they are
  dynamic and user-scoped.
- Residual risk we accept: the encryption key lives in an env var on the same
  host as the ciphertext, so a full host compromise yields both — this is
  defense-in-depth against a leaked DB backup / SQL read, not against root. The
  ciphertext carries a version byte so envelope/KMS encryption can be layered in
  later without a migration flag-day.
- A silently-skipped server in a headless task means the task quietly does less
  than intended; today that surfaces as a log line. A richer owner-visible
  notification is a follow-up.

## Alternatives considered

- **Mutate the shared client per turn** — rejected: a process-wide client cannot
  safely hold one user's bearer without leaking it to concurrent users, and
  add/remove churn is racy.
- **Adopt the official MCP Go SDK** for OAuth — rejected for this change: fleet
  has its own ~1,100-line MCP client; the discovery + token flow is a few hundred
  lines of standard-library code, and rolling it keeps the RFC 8707 `resource`
  parameter consistent on refresh (which `golang.org/x/oauth2`'s reusable
  `TokenSource` does not carry) and the dependency graph unchanged.
- **Client ID Metadata Documents (CIMD)** — the Nov-2025 spec addition — out of
  scope for the 2025-06-18 target; the registration logic sits behind an
  interface so it can be added later. DCR + a manual `client_id` fallback covers
  today's authorization servers.
- **Per-(user, server) dynamic client registration** — rejected: DCR identifies
  the client *app*, not the user, so we register once per (issuer, deployment)
  and share the `client_id`; only the tokens are per-user.
