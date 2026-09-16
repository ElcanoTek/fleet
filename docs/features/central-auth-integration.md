# Central Auth integration

Fleet keeps all three login paths independent:

- Fleet email/password remains available and is governed only by Fleet's user
  database and password-derived session epoch.
- The legacy `elcano_auth` magic-link cookie remains supported when
  `AUTH_SIGNING_PUBKEY` is configured.
- A central password Auth deployment can be configured through Fleet's existing
  OIDC Authorization Code + PKCE client.

For the central Auth service, set:

```text
FLEET_OIDC_ISSUER=https://auth.example.com
FLEET_OIDC_CLIENT_ID=fleet-client-id
FLEET_OIDC_CLIENT_SECRET=<one-time secret from auth app create>
FLEET_OIDC_SCOPES=openid email
```

Do **not** set `AUTH_SIGNING_PUBKEY` on a Fleet that signs in through a
password-mode Auth. That variable is the switch for the legacy magic-link
path above: the login page shows "Use Elcano email" whenever it is set, and
a password-mode Auth never mints the shared `elcano_auth` cookie, so that
button dead-ends. Fleet verifies Auth's signed back-channel logout tokens
from Auth's published `/jwks.json` (cached ten minutes, refreshed once when a
logout token names an unknown `kid`), so a signing-key rotation needs no
Fleet env edit and no static key is required. The trade-off is that Fleet
must be able to reach the Auth host when a logout arrives; Auth retries
failed deliveries for seven days. Set `AUTH_SIGNING_PUBKEY` only where the
legacy magic-link cookie is actually in use; it then also serves as an
offline fallback for logout verification.

With `FLEET_OIDC_AUTO_START=1`, an anonymous visit to `/login` first sends the
browser to Auth with `prompt=none`. A browser that already holds an Auth
session comes back with a code and lands in Fleet without a click; one that
does not is returned with `error=login_required` and Fleet shows its normal
login card, SSO button and password form both present, with no error banner.
`/login?manual=1` always shows the card, and a page carrying `?e=` (a login
error) never auto-starts, so the local admin password stays one URL away when
Auth is unreachable. Auth supports `prompt=none` from `1521760`+; an older Auth
ignores it and shows its own login form instead.

Logging out of Fleet ends the central session too. `POST /api/auth/logout`
clears Fleet's cookies as before and, whenever central Auth is configured,
sends the browser to `<FLEET_OIDC_ISSUER>/logout?client_id=…` (Auth's
RP-initiated logout) whatever kind of Fleet session it held: one minted
through Auth, one from the break-glass password, or none. Fleet cannot see
Auth's host-only cookie, so it cannot tell whether this browser also holds a
central session, and a person who signed in both ways expects one Sign out to
end both. Auth revokes every central session of the account (all devices),
fans a back-channel logout out to every registered application, and lands on
its own signed-out page; with no central session it lands there with nothing
to end. Without that step the very next visit would sign the user back in
silently. Only a deployment without central Auth lands on `/login?manual=1`.

The reverse direction is narrower on purpose: Auth's back-channel logout
(from Auth's own Sign out, Explorer or Lens) ends Fleet sessions that came
from Auth, but a Fleet session from the break-glass password stays valid.
That isolation is what makes the password a break-glass path.

Register the exact callback and signed logout endpoint on Auth:

```text
https://fleet.example.com/api/auth/oidc/callback
https://fleet.example.com/api/auth/backchannel-logout
```

Fleet sends client credentials with `client_secret_basic` when discovery
advertises it, as Auth does. Providers that do not advertise that method keep
the existing `client_secret_post` fallback.

Central sessions carry a separate Postgres-backed epoch keyed by OIDC issuer
and subject. A signed back-channel logout rotates only that epoch. Tokens must
carry `exp` (Auth signs a fresh `iat`/`exp` per delivery attempt). Duplicate
events are idempotent by JWT `jti`; Fleet-native password sessions and the
legacy magic-link route are not revoked or disabled. Every Fleet data request
for an OIDC session forwards the signed cookie's issuer, subject, and epoch to
both Go planes, which compare it with the live chat-store generation using a
read-only lookup; the generation row is created once, at login mint, and a
missing row fails closed. Replay ids are kept for seven days.

The back-channel endpoint is intentionally public at the browser proxy: it is a
server-to-server endpoint authenticated by Auth's Ed25519 signature, exact
issuer/audience, and event shape. Invalid tokens return 400; temporary backend
failures return 503 so Auth's durable delivery worker retries.
