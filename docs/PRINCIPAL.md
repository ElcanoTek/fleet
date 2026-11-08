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
| the chat listener's edge (`internal/httpapi/edge.go`) | a user API key on the listener's own port | `api_key` (`X-API-Key`) or `bearer` | the key's account and id |

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
`X-External-Issuer` or `X-External-Subject`. Its allowlist is empty.

**Phase 2: the internal service tokens are retired.**

- Nothing reads `X-Chat-Server-Token` or `X-Orchestrator-Server-Token` as a
  credential. A request that still presents one gets **401
  `{"error":"service_token_retired","message":…}`**, the message naming the
  replacement: on the chat listener from its edge, on the orchestrator from
  `RetiredServiceTokenMiddleware` (ahead of CSRF and every auth path).
- `FLEET_SERVER_TOKEN` and `ORCHESTRATOR_SERVER_TOKEN` (and the
  `CHAT_`/`CUTLASS_` spellings of the first) are removed settings
  (`envreg.removedSettings`). Like ADR-0106's removed aliases they read as
  unset whatever the environment holds; boot and `fleet doctor` warn about
  one still set (never printing it); `fleet config explain` notes it and
  `fleet config set-env` refuses it; and `fleet validate-config
  --retired-only` exits 7 on a bundle that still references one, so `fleet
  update` refuses to install over it. `config.Validate` no longer requires a
  server token, and `bootstrap.sh` no longer generates one.
- **User API keys.** `fleet sched apikey create <name> --type user --user
  <email>` mints a `fleet_user_…` key that names one account. The chat
  listener accepts it as `X-API-Key` or `Authorization: Bearer`; the account
  is resolved on every request (an unknown or disabled account is
  `not_a_member`). A user key carries no Operations Center permission and
  reaches no scoped-key route; any other key type is refused on the chat API
  with 403 `not_a_user_key`. Rotation and revocation are the ordinary key
  verbs. The HTTP `/keys` API refuses to mint one (the box's CLI names the
  account), so its request schema is unchanged.
- **Clients.** `fleet chat` and `fleet acp` read the key from `--key-file`,
  else `$FLEET_API_KEY`, else `~/.config/fleet/api-key`, never from argv, and
  send it as `X-API-Key`; `--email`/`$FLEET_USER_EMAIL` is now a check (a key
  for another account is refused before any turn), and the account is read
  from `GET /me`. The retired `--token-file` is refused with an error naming
  the replacement. fleet-bench takes the key from `--key-env` (default
  `FLEET_API_KEY`); `--token-env` is refused.
- **Caddy.** The `header_up` deletions of the retired headers stay in the
  rendered layout as defence in depth, so no installed Caddyfile drifts and
  `fleet doctor` rewrites nothing.

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

- **A new key type, not a per-user token store.** "Ordinary API keys" in the
  issue meant the existing keys; they name no person, which the chat API
  needs, so the key store gained a `user` type and `UserEmail` rather than a
  second credential system.
- **The Buzz gateway** (`fleet-buzz-gateway`, outside this repository)
  injected the server token and `X-User-Email`. It must send a user key as
  `X-API-Key` instead (docs/ACP.md); until it does, fleet answers it 401
  `service_token_retired`.

## Not done here

- Phase 3 (one router, one listener). Phase 4 (generated contracts, ADR-0109) shipped separately.
