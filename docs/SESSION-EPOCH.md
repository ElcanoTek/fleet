# Per-user session epoch (password reset ends outstanding sessions)

Resetting an account's password ends that account's existing sessions — on the
next request, on both the chat and Operations Center views. The invariant this
rests on (a correctly-signed session cookie is no longer sufficient to *be* a
session) is recorded in
[ADR-0041](adr/0041-mandatory-session-epoch-claim.md); this page is the shipped
design. The operator-facing summary of the three revocation levers lives in
[DEPLOYMENT.md](DEPLOYMENT.md).

## The value

`store.User.SessionEpoch` is the first 8 bytes of `SHA-256(password_hash)`, hex
encoded (16 chars). It is **derived, not stored**: bcrypt re-salts on every
write, so every password path moves the epoch with no bump call to remember
(the admin reset endpoint, `fleet chat user passwd`, `fleet admin add` on an
existing address, the legacy importer), a reset to the *same* password still
rotates it, and there is no migration, no backfill, and no window where a new
column exists but is empty.

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
| mint | `POST /api/auth/login`, `GET /api/auth/oidc/callback` | read `GET /auth/session-epoch`, stamp the claim into the HMAC cookie; a failed read **refuses the login** rather than minting a cookie the next request would reject |
| serve | `internal/httpapi` `/auth/session-epoch` | on `authMiddleware` alone (like `/auth/verify`) — the mint paths run before a session exists; an unprovisioned address gets the epoch of an empty hash, a real value no account can hold |
| forward | `chatServerHeaders`, `orchestratorHeaders` | `X-User-Session-Epoch` beside `X-User-Email`, over the same shared-token channel |
| check (chat) | `membershipMiddleware` | compared inside the `GetUser` it already performs — no extra query |
| check (ops center) | `headerTrustUser` → `checkSessionEpoch` | resolved through a chat-store lookup seam `cmd/fleet` injects; the two planes keep separate databases ([ADR-0005](adr/0005-separate-chat-and-sched-databases.md)), so this is a lookup, never a join — and it costs one chat-DB query per header-trust request |
| verdict | both backends | `401 {"error":"session_revoked"}` + `X-Session-Revoked: 1`; both Next proxies then delete the cookie (`sessionRevocation.ts`), or the still-valid signature would bounce every visit to `/login` back into a 401 |

Three rules keep the gate honest in the other direction:

- **A request with no claim is admitted.** The Ed25519 `elcano_auth` cookie is
  minted by the auth service, which chat cannot add a claim to, and a moc bearer
  is its own credential. Rejecting claimless requests would lock both out.
- **The Next tier refuses a chat-minted cookie with no claim**
  (`verifySessionToken`), which is what makes "no claim is admitted" safe: the
  only claimless requests that reach a backend are the two above.
- **A failed lookup is a 500, not a revocation.** An unreachable or slow chat
  store says nothing about the session; answering the revoked verdict would
  delete a valid cookie and sign the whole Operations Center out over a database
  blip. Same posture the membership lookup already takes on error.

## Deviation / honest scope

- **One forced re-login at deploy.** Cookies minted before this change carry no
  claim and are refused rather than grandfathered — a claimless cookie is
  precisely what the Go gate admits, so honouring it would leave a 14-day bypass
  of the whole mechanism.
- **No epoch bump independent of the password.** There is no "sign out my other
  devices" without a reset, because the epoch *is* a function of the password
  hash. That lever does not exist today either; adding a `session_epoch` column
  later is strictly additive.
- **Per account, not per device.** Two devices signed into one account share one
  epoch: a reset ends both (the point), and neither can be ended alone.
- **The Operations Center bearer login is untouched.** `fleet sched user
  passwd` re-upserts through `AddUser`, whose `ON CONFLICT` writes
  `session_token` back unchanged, so an issued bearer token survives a sched
  password change. Ending one means deleting (and re-creating) the sched user.
  A chat password reset never governed that credential.
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
