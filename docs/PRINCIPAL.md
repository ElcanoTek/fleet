# The request principal

Design note for #2050 ("One HTTP surface"), phases 1 and 2. The decision
record is [ADR-0108](adr/0108-typed-principal.md).

## What shipped

**Phase 1: a typed principal.** Every request reaches a chat or Operations
Center handler with a `principal.Principal` (`internal/principal`) in its
context, and handlers read identity from there:

| Who attaches it | When | `AuthMethod` | Carries |
|---|---|---|---|
| the gateway (`internal/webui/backend.go`) | a browser session's API call | `session` | the cookie's email and session claim (epoch; for central Auth also source, issuer, subject) |
| the gateway | a sign-in leg, a verified identity-provider event | `gateway` | the email being signed in or acted on |
| the gateway | the brand assets, a public shared conversation | `gateway` | nothing |
| the Operations Center's auth paths | the admin key, a scoped API key | `admin_key`, `api_key` | the key's id and permissions |
| a listener's edge (`edge.go` in `httpapi` and `sched/handlers`) | the legacy service token on the listener's own port | `service_token` | the email the caller named |

The gateway sends no identity header and no shared secret to a backend any
more. A backend resolves a user principal with `principal.Resolve`: one read
of the account's `core.users` row, the plane's membership rule, and the
session-epoch check, filling `UserID`, role and Operations Center scope from
the row. Chat (`membershipMiddleware`) and the Operations Center
(`userPrincipal`) share it, so the two planes cannot disagree about who is a
member or whether a session was revoked.

The sign-in legs (`/auth/verify`, `/auth/session-epoch`,
`/auth/external-session-epoch`, `/auth/external-session-revoke`,
`/auth/external-access`, `/auth/external-team-rename`) admit only the
gateway's principal (`gatewayMiddleware`).

The Operations Center's CSRF check exempts a `session` or `gateway` principal
(the gateway checked the browser's Origin before the call), and its rate
limiter counts any principal as an authenticated caller.

`TestNoIdentityHeaderReaders` fails the build on any non-test Go file that
spells `X-User-Email`, `X-User-Session-Epoch`, `X-User-Session-Source`,
`X-External-Issuer` or `X-External-Subject`. In phase 1 its allowlist holds
the two listener edges and the two clients of the service-token channel.

## Deviations from the issue

- **The Operations Center's key checks are not folded into one middleware.**
  The admin key and scoped-key paths (`verifyAdminKey`,
  `AdminOrUserAuthMiddleware`, `authorizeTaskCreator`, `HandleUpload`) keep
  their per-route semantics (401 vs 403, legacy `sk-` fall-through) and
  record a principal; manifest's contract checker still greps facts out of
  those functions (docs/MANIFEST-CONTRACT.md). "One authentication path"
  here means one place resolves a person: `principal.Resolve`.
- **The gateway does not resolve the account itself.** It has no database;
  it hands the backend an asserted principal and the backend resolves it.
- **`Principal.Scopes`** holds an API key's permissions. A person's access is
  their role and `OpsScope`.

## Not done here

- Phase 3 (one router, one listener) shipped separately: ADR-0110. Phase 4 (generated contracts, ADR-0109) shipped separately.
