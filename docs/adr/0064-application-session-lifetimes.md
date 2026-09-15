# ADR-0064: Fleet sessions live one day, idle out after twelve hours, and are re-minted on activity

- **Status:** Accepted
- **Date:** 2026-09-15
- **Deciders:** fleet maintainers, Elcano Auth owner

## Context

The `elcano_session` cookie ([ADR-0014](0014-oidc-sso-in-nextjs.md),
[ADR-0041](0041-mandatory-session-epoch-claim.md)) was a stateless HMAC over
`{email, exp, epoch, …}` valid for fourteen days from mint, with no notion of
inactivity: a laptop left open on Friday was still signed in the following
Friday, and the only thing that ended a session early was a password reset or a
central logout event moving the epoch.

Elcano Auth v2 changed what this cookie is for. Users now hold a **central**
session at the auth service (30 days absolute, 7 days idle) and each
application mints its own session from it through the OIDC code handoff. When
an application session lapses while the central one is live, the user is
redirected through the handoff and signed back in without a prompt. So the
application session no longer decides how often people log in. It decides two
other things: how long a stolen application cookie stays useful, and how often
the application re-checks with Auth that the account is still enabled and the
password unchanged. Both want a short number. Explorer and Lens settled on one
day absolute and twelve hours idle, and the Auth owner recorded that as the
convention for every Elcano application session
(`auth/docs/AUTH_V2_IMPLEMENTATION.md`, "Application session conventions").

Fleet was the odd one out, and it is stateless: there is no `last_seen_at` row
to touch, so an idle limit has to live in the cookie itself.

## Decision

**The `elcano_session` cookie carries an `idle` deadline alongside `exp`, both
are enforced on every request, and the proxy re-mints the cookie with a later
`idle` when the last mint is more than a minute old.**

- `exp` is the absolute deadline, one day from mint (`sessionAbsoluteSeconds`).
  It is copied on every re-mint, never extended. `idle` is
  `min(now + 12h, exp)` (`sessionIdleSeconds`).
- `verifySessionToken` refuses a token whose `idle` or `exp` has passed, and
  refuses a correctly signed token with **no** `idle` claim. Pre-deploy cookies
  are not grandfathered, for the same reason ADR-0041 did not grandfather
  claimless cookies: one signed for fourteen days flat is exactly what this
  decision removes.
- `refreshSessionCookie` runs in the request proxy on every authenticated
  pass-through (pages and `/api/*` alike). It re-signs the payload with the new
  `idle`, keeps `email`, `exp`, `epoch`, `source`, `issuer`, `subject`
  unchanged, and sets the cookie with `Max-Age` equal to the remaining absolute
  life. It does nothing when the deadline would move by less than
  `sessionTouchSeconds` (60), so a page's burst of requests costs one
  `Set-Cookie`, and nothing once `idle` already sits on `exp`. One minute is
  the Elcano touch convention shared with Auth, Explorer, and Lens.
- Both mint paths (password form, OIDC callback) go through one
  `signSessionPayload`, so neither can produce a cookie the verifier refuses.
- The auth service's own `elcano_auth` cookie (magic-link path) is untouched:
  Fleet holds only its public key and cannot re-mint it. Its lifetime is Auth's
  decision.

## Enforcement

- `web/src/app/lib/auth.test.ts` — mint deadlines; idle refusal ahead of the
  absolute deadline; refusal of a signed token without `idle`; no re-mint
  under a minute; re-mint after a minute preserves every identity claim and
  sets the expected cookie attributes; activity never moves `exp`; re-minting
  stops once `idle` is capped at `exp`; `elcano_auth` sessions are left alone;
  the `Secure` flag follows the request.
- `web/src/proxy.test.ts` — the proxy touches the cookie exactly once on an
  authenticated pass-through and never on redirects, 401s, public routes, or
  bearer-only requests.

## Consequences

- **Every logged-in user is signed out once at deploy.** Central-Auth users
  are signed back in by the handoff without a prompt; password users log in
  once more.
- A stolen Fleet cookie is worth at most one day, and at most twelve hours if
  the victim stops using it. Before, fourteen days.
- Fleet re-runs the OIDC handoff at least daily per user, so a disabled Auth
  account or rotated password is caught within a day even if a back-channel
  logout delivery were lost.
- The proxy now writes a `Set-Cookie` on roughly one authenticated request per
  user per minute. It is a signing operation on an already-imported HMAC key;
  no storage is involved and the tier stays stateless.
- Deployments that want different numbers change the two constants; they are
  deliberately not settings, because the values are a cross-service
  convention rather than a per-box policy.
