# ADR-0108: Typed Principal; internal service tokens retired

- **Status:** Accepted
- **Date:** 2026-11-08
- **Deciders:** fleet maintainers
- **Supersedes:** [ADR-0053](0053-public-api-through-the-tls-front.md) (its routing
  stands; its header-trust channel, and the Caddy strip that guarded it, no
  longer carry any trust)
- **Issue:** #2050 ("One HTTP surface"), phases 1 and 2
- **Design note:** [docs/PRINCIPAL.md](../PRINCIPAL.md)

## Context

Since ADR-0091 the browser gateway (`internal/webui`) calls the chat and
Operations Center handlers in-process, but identity still travelled between
those layers the way it did when Next.js proxied over loopback: the gateway
wrote `X-User-Email`, the session-epoch claim (`X-User-Session-Epoch`) and,
for a central-Auth session, `X-User-Session-Source`, `X-External-Issuer` and
`X-External-Subject` onto a fresh header set for each call, next to a shared
secret (`X-Chat-Server-Token` / `X-Orchestrator-Server-Token`, both
`FLEET_SERVER_TOKEN`) that told the backend to believe them. Each backend
re-read them in its own auth stack: chat's `authMiddleware` and
`membershipMiddleware`, the Operations Center's `headerTrustUser` and
`checkSessionEpoch`.

That shape had three costs. A header is a string any layer can set, so the
guarantee "a browser can never present an identity" rested on every gateway
route building its header set from scratch. The same lookup (account row,
membership, epoch) was written twice. And the same channel stayed open on the
listeners' own loopback ports for any process holding the token — `fleet
chat`, `fleet acp`, fleet-bench — which is why ADR-0053 must strip the headers
at Caddy.

## Decision

1. **Identity is a typed `principal.Principal` in the request context.**
   `internal/principal` defines it: email, `UserID`, role, Operations Center
   scope, an API key's id and permissions, the `AuthMethod` (`session`,
   `gateway`, `api_key`, `bearer`, `admin_key`) and a session's claim (epoch,
   source, issuer, subject). A request context cannot be written by a network peer,
   so a principal is only ever there because fleet code put it there:
   - the gateway attaches one to every in-process call — a browser session's
     asserted principal, or its own (`gateway`) for the sign-in legs, a
     verified identity-provider event and the identity-less brand assets —
     and no identity header;
   - a listener's edge middleware attaches one for a credential the request
     presented on that listener's own port.

   Backends read identity from the principal only. **One lookup** resolves
   it: `principal.Resolve` reads the account's `core.users` row once,
   applies the plane's membership rule (chat: enabled; Operations Center:
   Operations Center access, or any enabled account on the Prompt Library),
   checks a session's epoch claim, and returns the principal with `UserID`,
   role and scope filled from the row. Chat's `membershipMiddleware` and the
   Operations Center's `userPrincipal` both call it. Membership outranks the
   session; a lookup failure is a 500, never a revocation (unchanged from
   ADR-0041).

   The sign-in legs that act on an email the caller names (`/auth/verify`,
   `/auth/session-epoch`, `/auth/external-*`) admit only the gateway's own
   principal, so no user principal can mint an epoch or apply provider
   access for another address. CSRF and the rate-limit bucket choice key off
   the principal's `AuthMethod`: a `session` or `gateway` principal passed
   the gateway's Origin check and is exempt from the Operations Center's.

2. **The internal service-token channel is retired.** `FLEET_SERVER_TOKEN`
   and `ORCHESTRATOR_SERVER_TOKEN` are no longer settings, and nothing reads
   `X-Chat-Server-Token`, `X-Orchestrator-Server-Token` or an identity header.
   A request that still presents a service token gets **401
   `service_token_retired`** with a message naming the replacement — on the
   chat listener from its edge, on the orchestrator ahead of CSRF and every
   auth path — never a silent fall-through. The retired names follow the
   configuration registry's retirement path (ADR-0106): they read as unset
   whatever the environment holds; boot, `fleet doctor`, `fleet config
   explain`/`set-env` report them with the advice; and `fleet validate-config
   --retired-only` (the pre-install check of `fleet update` and bootstrap)
   exits 7 on a bundle that still references one. `bootstrap.sh` no longer
   generates `FLEET_SERVER_TOKEN`. The Caddy `header_up` deletions of these
   headers stay in the rendered layout as harmless defence in depth, so no
   installed Caddyfile drifts.
3. **`fleet chat`, `fleet acp` and fleet-bench authenticate with a user API
   key.** A new key type, `user` (`fleet_user_…`, minted with `fleet sched
   apikey create <name> --type user --user <email>`), names one account
   (`APIKey.UserEmail`). The chat listener's edge accepts it as `X-API-Key`
   or `Authorization: Bearer` and attaches an `api_key`/`bearer` principal
   for that account, which the membership gate then resolves like any other
   — so disabling the account ends the key's access. A user key carries no
   Operations Center permission and reaches no scoped-key route
   (`x-fleet-key-types.user` is `{permissions: [], methods: []}`); the other
   key types name no user and are refused on the chat API with 403
   `not_a_user_key`. The clients read the key from `--key-file`, else
   `$FLEET_API_KEY`, else `~/.config/fleet/api-key`, never from argv; a
   user key takes no per-key rate limit (the chat's per-user limit applies).
   Which key type authenticates on which surface (chat, orchestrator, A2A,
   triggers) is one table, `apikeys.KeyType.AllowsSurface`, that every edge
   consults through `ValidateKeyOn`, so a user key cannot reach A2A or the
   orchestrator; and the key store re-reads its file on any change, so a
   revoke or rotate made by the CLI takes effect on the server's next request;
   `--email` only checks which account the key acts as. The HTTP `/keys`
   API does not mint user keys (the CLI on the box does), so the public
   API's request contract is unchanged.

## Enforcement

- `internal/principal/guard_test.go` (`TestNoIdentityHeaderReaders`) fails on
  any non-test Go file under `cmd/` or `internal/` that spells an identity
  header. Its allowlist is empty.
- `internal/httpapi/auth_test.go`, `user_key_client_test.go`: the chat edge
  (retired token 401, no credential 401, a key naming no user 403, a user key
  as X-API-Key and as bearer) and `fleet chat`'s client against the real
  listener; `cmd/fleet/csrf_coverage_test.go` and
  `internal/sched/handlers/header_trust_test.go`: the orchestrator's 401.
- `internal/sched/apikeys/sync_test.go` (a revoke, rotate, delete or edit by
  another process takes effect at once; `TestKeyTypeSurfaces`),
  `edge_guard_test.go` (`TestKeyEdgesNameTheirSurface`: no edge validates a
  key without its surface), and `internal/sched/handlers/key_surface_test.go`
  (key types × orchestrator edges).
- `internal/config/envreg/retired_lookup_test.go`,
  `internal/config/retired_aliases_test.go`,
  `cmd/fleet/validate_config_test.go`
  (`TestValidateConfigRetiredOnlyReportsTheServiceToken`): the retirement
  path.
- `internal/principal/principal_test.go`: `Resolve`'s verdicts and order.
- Spoofing: `internal/httpapi/principal_spoof_test.go`,
  `internal/webui/principal_spoof_test.go`,
  `internal/sched/handlers/principal_methods_test.go`.
- `internal/httpapi/session_epoch_test.go`,
  `internal/sched/handlers/session_epoch_test.go`: revocation through the
  principal on both planes.

## Consequences

- A handler that needs to know who is calling reads `principal.From`; there
  is no header to read and the guard test refuses one.
- No shared secret lets any caller name a user. A process on the box that
  wants to act as a person needs that person's key, which an admin mints and
  can revoke on its own.
- Every existing setup of `fleet chat`, `fleet acp`, fleet-bench and the
  Buzz `fleet-buzz-gateway` that relied on the server token must move to a
  user key; until it does it gets the 401 that says how.
- The Operations Center's API-key checks (`verifyAdminKey`, the scoped-key
  paths in `AdminOrUserAuthMiddleware`, `authorizeTaskCreator`,
  `HandleUpload`) stay where they are and now also record an `api_key` or
  `admin_key` principal. They are not folded into one middleware here:
  manifest's contract checker reads facts out of those functions
  (docs/MANIFEST-CONTRACT.md) until it moves to the OpenAPI spec.

## Alternatives considered

- **Resolve the account in the gateway and hand the backends a verified
  principal.** It would give the gateway a database dependency it does not
  have and leave the listeners' own ports needing the same code anyway.
  `Resolve` is shared instead, and the gateway's principal is an assertion
  the backend verifies.
- **Sign the identity headers.** Keeps a string channel and a secret where a
  context value needs neither.
