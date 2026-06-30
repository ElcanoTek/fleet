# ADR-0014: OIDC / OAuth2 SSO lives in the Next.js layer, not the chat server

- **Status:** Accepted
- **Date:** 2026-06-30
- **Deciders:** fleet maintainers

## Context

Issue #240 asks for OIDC / OAuth2 single sign-on. Its original blueprint placed
the flow on the Go chat server. That contradicts the **shipped** auth
architecture: the chat server is never exposed to browsers — it trusts only
`X-Chat-Server-Token` + `X-User-Email` from the trusted Next.js proxy and scopes
every query by email (see `internal/httpapi/auth.go`). Login, session cookies,
and the existing "Use Elcano email" magic-link handoff **already** live in the
Next.js layer (`web/src/app/lib/auth.ts`, `web/src/app/api/auth/*`). Putting an
OAuth redirect/callback on the chat server would mean exposing it to the browser
and forking a second front-channel auth path — exactly the kind of parallel,
weaker path the project forbids.

The ticket notes were also flagged as potentially stale; we build to the real
code, not the prose.

## Decision

Implement OIDC entirely in the Next.js layer as an **Authorization Code + PKCE**
flow for a confidential client, and have a successful login **mint the same
`elcano_session` HMAC cookie** the password form mints. Two route handlers:

- `GET /api/auth/oidc/start` — mints `state` + `nonce` + a PKCE verifier, stores
  them in short-lived httpOnly `SameSite=Lax` cookies, and 303s the browser to
  the IdP's authorization endpoint (discovered from
  `FLEET_OIDC_ISSUER/.well-known/openid-configuration`).
- `GET /api/auth/oidc/callback` — validates `state` against the cookie (CSRF),
  exchanges `code` (+ PKCE verifier + client secret) at the token endpoint,
  validates the ID token's claims, enforces an optional email-domain allowlist,
  mints `elcano_session`, and 303s home.

Because the session cookie is the existing one, **everything downstream is
unchanged**: middleware, `getServerSession`, and — critically — the chat-server
**membership gate**. SSO proves *who you are*; the chat user-list still decides
*who may use chat* (an authenticated-but-unprovisioned email lands on the
no-access page, identical to the magic-link path). This deliberately does **not**
auto-provision users — that would fork a new write path and is left as a
documented follow-up.

**ID-token signature is intentionally not re-verified.** The token is read
directly from the token endpoint over a server-to-server TLS channel, so per
OIDC Core §3.1.3.7 the client MAY rely on TLS for integrity and validate only
the claims (`iss`, `aud`, `exp`, `nonce`, and `azp` when present). This is what
lets SSO ship with **zero new crypto dependencies**. `decodeJwtClaims` carries a
comment forbidding its use on any front-channel (e.g. implicit-flow) token, where
that assumption would not hold.

Configuration is runtime env (`FLEET_OIDC_*`), resolved server-side so the login
page renders the SSO button only where the flow is actually configured — a pure
ops switch, mirroring the `AUTH_SIGNING_PUBKEY` gate for the magic-link button.
The flow is **disabled** unless `FLEET_OIDC_ISSUER` + `FLEET_OIDC_CLIENT_ID` +
`FLEET_OIDC_CLIENT_SECRET` are all set; a partial config is treated as off.

## Enforcement

- `web/src/app/lib/oidc.test.ts` — config parsing/defaults, domain allowlist,
  claim validation (issuer/audience/azp/exp/nonce/email_verified), discovery
  caching + failure non-caching, PKCE determinism.
- `web/src/app/api/auth/oidc/start/route.test.ts` — authorize-URL params + temp
  cookies; disabled and discovery-failure bounces.
- `web/src/app/api/auth/oidc/callback/route.test.ts` — happy path mints
  `elcano_session` (verified via `verifySessionToken`); CSRF state mismatch does
  **not** exchange the code; nonce mismatch, token-endpoint failure, provider
  `error`, domain allowlist (allow + deny), and disabled-config cases.
- `web/src/app/login/login-card.test.tsx` — the SSO button is gated and points
  at `/start`.
- `web/middleware.ts` lists both legs in `publicApiPaths` (they are pre-session
  by definition); the build + the `force-dynamic` login page keep the toggle a
  runtime switch.

## Consequences

- SSO is a turnkey ops switch with no Go changes and no new dependencies; the
  attack surface added is two pre-session route handlers, both bounded by
  state/nonce/PKCE and a coarse, non-enumerating error vocabulary.
- Relying on TLS rather than ID-token signature verification is sound only for
  the **code flow with a direct token-endpoint exchange**. If fleet ever adds an
  implicit/hybrid front-channel path, that path MUST verify signatures (JWKS) —
  the `decodeJwtClaims` comment records this boundary.
- Users still need provisioning in the chat user-list. SSO removes the password,
  not the membership decision. Auto-provisioning (an opt-in that upserts a
  passwordless user from a verified, allow-listed email) is a clean follow-up
  once it can be done without weakening the membership gate.

## Alternatives considered

- **OIDC on the Go chat server (the ticket's blueprint).** Rejected: it would
  expose the chat server to browsers and fork a second, weaker auth path,
  violating the "one trusted proxy" and "one governed path" invariants.
- **Add `next-auth`/Auth.js or `jose`.** Rejected for v1: a dependency with its
  own session model and migration cost, where ~250 lines of well-tested route
  code over the existing session machinery suffices. `jose` would only be needed
  for front-channel signature verification, which the code flow does not require.
- **Auto-provision on first SSO login.** Deferred, not rejected: valuable, but it
  introduces a new user-write path that deserves its own change + ADR rather than
  riding in on the auth flow.
