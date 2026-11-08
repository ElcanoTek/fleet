# Per-user session epoch (password reset ends outstanding sessions)

Resetting an account's password ends that account's existing sessions — on the
next request, on both the chat and Operations Center views. The invariant this
rests on (a correctly-signed session cookie is no longer sufficient to *be* a
session) is recorded in
[ADR-0041](adr/0041-mandatory-session-epoch-claim.md); this page is the shipped
design. The operator-facing summary of the three revocation levers lives in
[DEPLOYMENT.md](DEPLOYMENT.md).

## The value

`store.User.SessionEpoch` is the first 8 bytes of
`SHA-256(password_hash || session_salt)`, hex encoded (16 chars). It is
**derived**: bcrypt re-salts on every write, so every password path moves the
epoch with no bump call to remember (the admin reset endpoint, `fleet chat user
passwd`, `fleet admin add` on an existing address, the legacy importer), and a
reset to the *same* password still rotates it. `users.session_salt` (migration
060, default `''`) is the one other writer: a central Auth sign-out delivered
over the back-channel rotates it so the account's Fleet password sessions end
too, without a password change. The empty default keeps the pre-060 value
byte-identical, so that migration signed nobody out.

Reads derive it in SQL (`sessionEpochExpr`), so `password_hash` never leaves
Postgres on the request path — `GetUser` runs on every authenticated request and
`ListUsers` covers every account at once. The Go twin (`sessionEpochFor`) serves
the two callers that legitimately hold a hash already: `CreateUser`, and the
unknown-email answer, whose input is the empty string. A store test pins the two
derivations byte-identical.

The claim travels in the cookie, so it is a truncated digest rather than the
hash itself: a one-way value over a string that already carries a 128-bit random
salt, and not a credential `/auth/verify` would accept.

## Mint → forward → check

| Step | Where | Behaviour |
|---|---|---|
| mint | `POST /api/auth/login`, `GET /api/auth/oidc/callback` | password login reads `/auth/session-epoch`; OIDC reads `/auth/external-session-epoch` and stamps issuer+subject+source with its independent claim; either failed read refuses login |
| serve | `internal/httpapi` `/auth/session-epoch` | on `authMiddleware` alone (like `/auth/verify`) — the mint paths run before a session exists; an unprovisioned address gets the epoch of an empty hash, a real value no account can hold |
| forward | `chatServerHeaders`, `orchestratorHeaders` | `X-User-Session-Epoch` beside `X-User-Email`; OIDC also forwards source, issuer, and subject over the same shared-token channel |
| check (chat) | `membershipMiddleware` | compared inside the `GetUser` it already performs — no extra query |
| check (ops center) | `headerTrustUser` → `checkSessionEpoch` | read straight from the one identity table, `core.users` / `core.external_auth_epochs` (`identity.SessionEpochSQL`; #2049 M4, [ADR-0105](adr/0105-one-identity-table.md)) — one query per header-trust request, no seam |
| verdict | both backends | `401 {"error":"session_revoked"}` + `X-Session-Revoked: 1`; both Next proxies then delete the cookie (`sessionRevocation.ts`), or the still-valid signature would bounce every visit to `/login` back into a 401 |

Three rules keep the gate honest in the other direction:

- **A request with no claim is admitted.** The Ed25519 `elcano_auth` cookie is
  minted by the auth service, which chat cannot add a claim to. Rejecting
  claimless requests would lock it out. (The Operations Center's own password
  bearer, which also carried no claim, is retired: ADR-0105.)
- **The Next tier refuses a chat-minted cookie with no claim**
  (`verifySessionToken`), which is what makes "no claim is admitted" safe: the
  only claimless requests that reach a backend are the one above.
- **A failed lookup is a 500, not a revocation.** An unreachable or slow chat
  store says nothing about the session; answering the revoked verdict would
  delete a valid cookie and sign the whole Operations Center out over a database
  blip. Same posture the membership lookup already takes on error.

## Deviation / honest scope

- **One forced re-login at deploy.** Cookies minted before this change carry no
  claim and are refused rather than grandfathered — a claimless cookie is
  precisely what the Go gate admits, so honouring it would leave a 14-day bypass
  of the whole mechanism.
- **Password and central epochs relate one way.** A Fleet password reset
  rotates only the password epoch; OIDC sessions keep their stored
  issuer+subject generation. A signed central logout event (idempotent by
  `jti`) rotates that generation AND the account's `session_salt`, so it ends
  the account's Fleet password sessions too: "signed out of every app" means
  every app. Where passwords are on (no central Auth, or
  `FLEET_OIDC_ALLOW_PASSWORD=1`), the password remains a way to sign in
  afterwards; a central-Auth Fleet otherwise refuses password sessions
  outright (ADR-0095).
- **Per account, not per device.** Two devices signed into one account share one
  epoch: a reset ends both (the point), and neither can be ended alone.
- **There is no separate Operations Center credential.** Its password login
  (`POST /auth/login`, `POST /auth/logout`) and the bearer token it minted are
  retired (#2049 M4, [ADR-0105](adr/0105-one-identity-table.md)): people reach
  the Operations Center with the same session cookie, so the same epoch ends
  both, and programs use API keys, which epochs do not govern.
- **Magic-link (`elcano_auth`) sessions carry no epoch** and stay revocable only
  at the auth service that mints them.
- **Next-only routes are not gated.** The epoch is enforced by the two
  backends, so the few endpoints that authorize on `getServerSession()` alone
  and never call a backend — `/api/session`, `/api/model-catalog`,
  `/api/model-rankings`, `/api/catwalk-models`, `/api/model-check` — keep
  answering a revoked cookie until some other call returns `session_revoked`
  and `dropRevokedSession` removes it. They serve the caller's own email and
  public model catalogs: no client data, no budget spend. Both UIs reach a
  backend within the first render, so the window is short in practice.
- **A stream already open finishes its turn.** The epoch is checked when a
  request connects, so an SSE stream opened before the reset
  (`/api/chat`, `/api/conversations/[id]/stream`, the task run-log stream) keeps
  delivering until that turn ends. The next connect is refused.
- **No revocation list and no server-side session store.** The cookie stays
  stateless; revocation works by invalidating what it *claims*.
