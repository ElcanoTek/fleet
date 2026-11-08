# ADR-0108: Typed Principal; internal service tokens retired

- **Status:** Proposed. Decision 1 is implemented (#2050 phase 1); decisions 2 and 3 land with phase 2, which marks this record Accepted.
- **Date:** 2026-11-08
- **Deciders:** fleet maintainers
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
   `gateway`, `api_key`, `bearer`, `admin_key`, plus the legacy
   `service_token` until decision 2) and a session's claim (epoch, source,
   issuer, subject). A request context cannot be written by a network peer,
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

2. **The internal service-token channel is retired** (phase 2).
3. **`fleet chat`, `fleet acp` and fleet-bench authenticate with an API key**
   (phase 2).

## Enforcement

- `internal/principal/guard_test.go` (`TestNoIdentityHeaderReaders`) fails on
  any non-test Go file under `cmd/` or `internal/` that spells an identity
  header. Its allowlist holds only the legacy service-token edges and their
  clients until decision 2 empties it.
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
- The gateway no longer needs `FLEET_SERVER_TOKEN` or
  `ORCHESTRATOR_SERVER_TOKEN`; the latter is read by nothing.
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
