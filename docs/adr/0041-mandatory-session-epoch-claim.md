# ADR-0041: Session cookies carry a mandatory session-epoch claim

- **Status:** Accepted
- **Date:** 2026-08-04
- **Deciders:** fleet maintainers

## Context

The `elcano_session` cookie is a stateless HMAC over `{email, exp}` with a
14-day life, minted and verified inside the Next.js tier
([ADR-0014](0014-oidc-sso-in-nextjs.md) put both login paths there), and the
only server-side gate was the user-list check in `membershipMiddleware`. So a
valid signature over a still-provisioned email *was* a session, and nothing
recorded when that session was issued.

That made the standard response to a stolen cookie — an admin resets the
compromised account's password — a no-op: the reset changed what a future login
needs and left every issued cookie fully valid for the rest of its 14 days. The
only working levers were deleting the account (which takes the user's access to
their own data with it) or rotating `APP_SESSION_SECRET` (which logs out
everybody). The same cookie authenticates the Operations Center, so the gap
covered both views.

## Decision

**A session cookie must carry a session-epoch claim, and a request is honoured
only while that claim still matches the account.** A correct signature is no
longer sufficient.

- The epoch is derived from the account's stored bcrypt hash
  (`hex(sha256(password_hash)[:8])`), so every password write moves it and no
  write path has to remember to bump anything.
- Both mint paths read it from `GET /auth/session-epoch` and stamp it into the
  cookie; a failed read refuses the login rather than minting a cookie the next
  request would reject.
- `verifySessionToken` refuses a correctly-signed token with no claim: cookies
  predating this decision are **not** grandfathered, because a claimless cookie
  is exactly what the backends still admit.
- Both backends compare the forwarded claim — chat inside the user lookup it
  already performs, the Operations Center through a chat-store lookup seam
  `cmd/fleet` injects, since the two planes keep separate databases
  ([ADR-0005](0005-separate-chat-and-sched-databases.md)). A mismatch is
  `401 session_revoked`; a lookup failure is a 500, never a revocation.
- Requests with **no** claim stay admitted, because the auth service's Ed25519
  `elcano_auth` cookie cannot carry one; those sessions are revoked at the
  service that mints them.

## Enforcement

- `internal/store/session_epoch_test.go` — the epoch moves on every password
  write (including a reset to the same password), survives role/team edits, and
  the SQL and Go derivations agree byte for byte.
- `internal/httpapi/session_epoch_test.go` — a session minted before a reset is
  refused afterwards on every gated route while a fresh one works; a forged
  claim is refused; non-membership still outranks the epoch.
- `internal/sched/handlers/session_epoch_test.go` — the same verdicts on the
  Operations Center's header-trust path, including all three `headerTrustUser`
  callers — the auth middleware plus the two routes outside it (task create,
  upload) — and the 500-not-revocation rule when the chat store is down.
- `web/src/app/lib/auth.test.ts` — a claimless token is refused;
  `chatServer.test.ts` / `mocServer.test.ts` — the claim is forwarded and the
  revoked verdict drops the cookie; `api/auth/login/route.test.ts` — the mint
  path stamps the epoch and refuses to mint without one.

## Consequences

- Password reset becomes the incident-response lever for a stolen cookie, with
  blast radius of exactly one account, on both views, within one request.
- **Every logged-in user must sign in once more at deploy** — the cost of not
  grandfathering claimless cookies.
- The web tier hard-depends on a chat-server serving `/auth/session-epoch`:
  version skew or a restarting backend is a login outage, not a degraded login.
- The epoch cannot be bumped independently of the password, so "sign out my
  other devices" still does not exist. Adding a `session_epoch` column later is
  strictly additive.
- The Operations Center pays one chat-DB lookup per header-trust request; the
  chat plane pays nothing (the comparison rides an existing query).
- Remaining carve-outs are written down in [`../SESSION-EPOCH.md`](../SESSION-EPOCH.md):
  magic-link sessions, the Operations Center bearer token, and a stream already
  open when the reset lands.

## Alternatives considered

- **A `session_epoch` column bumped on password change.** Rejected for v1: it
  needs a migration plus a backfill, and every password path has to remember to
  bump it — the derived value cannot be forgotten. It buys an
  independent-of-password bump, which nothing asks for yet.
- **A server-side session store (revocation list).** Rejected: it replaces a
  stateless cookie with a per-request store lookup and a new expiry/GC surface,
  to solve a problem one derived claim already solves.
- **Grandfather claimless cookies for the remainder of their life.** Rejected:
  the backends admit claimless requests by design (the magic-link path), so
  honouring them in the Next tier would leave a 14-day bypass of the mechanism.
- **Put the check only in the chat plane.** Rejected once measured: the same
  cookie reaches ~40 Operations Center routes, so a chat-only check would have
  left the reset partial on the account class most likely to be attacked.
