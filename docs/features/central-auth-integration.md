# Central Auth integration

Fleet has three ways to sign in:

- Fleet email/password, governed by Fleet's user database and
  password-derived session epoch. **Off whenever central Auth is configured**
  (below), unless `FLEET_OIDC_ALLOW_PASSWORD=1`, or `FLEET_OIDC_GENERIC=1`
  marks the SSO provider as another OIDC identity provider.
- The legacy `elcano_auth` magic-link cookie, when `AUTH_SIGNING_PUBKEY` is
  configured.
- A central password Auth deployment, through Fleet's OIDC Authorization
  Code + PKCE client.

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

## SSO only: Auth signs people in and manages them

Configuring central Auth (the issuer, client id and client secret above, the
same three values that make Fleet offer SSO at all) turns Fleet's own
passwords off and hands account management to Auth
([ADR-0095](../adr/0095-central-auth-fleets-have-no-passwords.md)). There is
no separate switch to forget. Both decisions are made once at startup from
configuration, never from whether Auth answers, and the browser gateway,
chat-server and the Operations Center all apply them:

- The login card shows the SSO button as its only action (and "Use Elcano
  email" when `AUTH_SIGNING_PUBKEY` is set); there is no password form, on
  `/login?manual=1` either. A password form posted anyway is refused with
  `?e=password_off` before any password is checked, and chat-server's
  `/auth/verify` refuses too.
- A session cookie minted by a password login stops working at once, whether
  it predates the switch or not. A browser that also holds a valid
  `elcano_auth` cookie is signed in by that one instead.
- The Operations Center's `POST /auth/login` (and its public `/v1/auth/login`)
  answers 403, and a bearer it minted earlier is refused by the auth
  middleware, task creation and upload alike.
- **Accounts, roles and teams are managed in Auth.** Settings → Admin → Users
  is the view-only usage list (no Add user, no row editor with roles, team,
  password reset or delete, no Rename team) with a link to Auth's console,
  and Settings → Team shows a person's team without Create or Leave.
  Chat-server refuses each of those writes (409, saying where to make the
  change): Auth adds people through its Fleet grant
  ([CENTRAL-AUTH-PROVISIONING.md](../CENTRAL-AUTH-PROVISIONING.md)), sends
  their Chat and Ops roles, and with team sync on sends their team. A team
  renamed in Auth is renamed here the same share-keeping way an admin's
  Rename team did ([CENTRAL-AUTH-PROVISIONING.md](../CENTRAL-AUTH-PROVISIONING.md#team-renames)).
  A change made here would not reach Auth, and Auth's next push would
  quietly undo it.
- **One way: Auth to Fleet.** Auth is the single source of truth and pushes
  to Fleet; nothing flows back. Do not point Fleet's account-events feed
  ([ACCOUNT-EVENTS.md](../ACCOUNT-EVENTS.md)) at Auth on such a deployment:
  Fleet's account pages change nothing, and a change root makes with the CLI
  stays local until Auth's next push for that person overwrites it.
- **Team sync is part of this.** Since Fleet no longer changes teams, Auth's
  team sync must be on for the Fleet application, or teams cannot be changed
  anywhere. `production-support`'s `register-auth-apps.sh` switches it on when
  it registers a Fleet that has no teams yet, so the teams set in Auth win.
  A Fleet that already has teams needs a one-time import into Auth first,
  after its people are granted Fleet there (Auth's DEPLOY.md, "Fleet team
  sync"); the script stops and says so rather than guessing.
- **Removing someone.** Removing Fleet from their access in Auth signs them
  out of both planes and keeps their data (reversible). Erasing their Fleet
  data is a deliberate root command on the server, `fleet admin rm <email>`:
  Auth is never able to delete Fleet data.
- Unchanged: central SSO sessions, the `elcano_auth` cookie, machine API keys,
  webhook signatures, the operator's shared-token channel and every root CLI
  command (`fleet admin add`, `fleet chat user passwd`, …). Password rows are
  kept; they are just not accepted.

**Before the first update that carries this**, check that everyone who signs
in to a client's Fleet has an Auth account under the same email with Fleet
granted. Anyone who does not is locked out until they get one (their Fleet
data is kept and is theirs at their first SSO sign-in). To give them time,
set `FLEET_OIDC_ALLOW_PASSWORD=1` before updating and remove it later.

**Another OIDC provider.** `FLEET_OIDC_*` also works with an identity
provider that is not central Auth (Okta, Entra, ...). Such a provider proves
who someone is but sends Fleet no accounts, roles or teams, so set
`FLEET_OIDC_GENERIC=1`: that Fleet keeps its own passwords and account
management (people are added in Settings → Admin → Users or with
`fleet admin add`), and none of the above applies. Leave it unset for central
Auth. Like the break-glass, it is read at startup.

**Break-glass.** When Auth is down nobody can sign in to the web UI. Root sets
`FLEET_OIDC_ALLOW_PASSWORD=1` in `/etc/fleet/fleet.env`, runs `fleet restart`,
signs in with a Fleet password (`fleet admin add <email>` sets one), and
removes the line and restarts again once Auth is back. It turns passwords
back on only: accounts stay managed in Auth (use the root CLI meanwhile). The setting is read at
startup only, so every change to it (or to the `FLEET_OIDC_*` values) takes a
restart; a config reload ([CONFIG-RELOAD.md](../CONFIG-RELOAD.md)) never
applies it. The boot log says when passwords are off.

## Auto-start

With `FLEET_OIDC_AUTO_START=1`, an anonymous visit to `/login` first sends the
browser to Auth with `prompt=none`. A browser that already holds an Auth
session comes back with a code and lands in Fleet without a click; one that
does not is returned with `error=login_required` and Fleet shows its normal
login card with no error banner: just the SSO button, or the SSO button and
the password form where `FLEET_OIDC_ALLOW_PASSWORD=1`. `/login?manual=1`
always shows the card, and a page carrying `?e=` (a login error) never
auto-starts, so a failed attempt cannot loop. Auth supports `prompt=none` from
`1521760`+; an older Auth ignores it and shows its own login form instead.

Logging out of Fleet ends the central session too. `POST /api/auth/logout`
clears Fleet's cookies as before and, whenever central Auth is configured,
sends the browser to `<FLEET_OIDC_ISSUER>/logout?client_id=…` (Auth's
RP-initiated logout) whatever kind of Fleet session it held: one minted
through Auth, one from a break-glass password, or none. Fleet cannot see
Auth's host-only cookie, so it cannot tell whether this browser also holds a
central session, and a person who signed in both ways expects one Sign out to
end both. Auth revokes every central session of the account (all devices),
fans a back-channel logout out to every registered application, and lands on
its own signed-out page; with no central session it lands there with nothing
to end. Without that step the very next visit would sign the user back in
silently. Only a deployment without central Auth lands on `/login?manual=1`.

The reverse direction ends everything too: Auth's back-channel logout (from
Auth's own Sign out, Explorer or Lens) rotates both the external epoch and the
account's password session salt, so Fleet sessions that came from Auth AND a
Fleet session from a break-glass password end within seconds. Where
`FLEET_OIDC_ALLOW_PASSWORD=1` keeps passwords on, the password itself keeps
working (it is a way to sign in, not a session that survives a sign-out).

Register the exact callback and signed logout endpoint on Auth:

```text
https://fleet.example.com/api/auth/oidc/callback
https://fleet.example.com/api/auth/backchannel-logout
```

Fleet sends client credentials with `client_secret_basic` when discovery
advertises it, as Auth does. Providers that do not advertise that method keep
the existing `client_secret_post` fallback.

Central sessions carry a separate Postgres-backed epoch keyed by OIDC issuer
and subject. A signed back-channel logout rotates that epoch AND the account's
password session salt (migration 060), so Fleet-native password sessions for
the same email end as well; the legacy magic-link route is untouched (revoke
those at the auth service that mints them). Tokens must carry `exp` (Auth
signs a fresh `iat`/`exp` per delivery attempt). Duplicate events are
idempotent by JWT `jti`. Every Fleet data request
for an OIDC session forwards the signed cookie's issuer, subject, and epoch to
both Go planes, which compare it with the live chat-store generation using a
read-only lookup; the generation row is created once, at login mint, and a
missing row fails closed. Replay ids are kept for seven days.

The back-channel endpoint is intentionally public at the browser proxy: it is a
server-to-server endpoint authenticated by Auth's Ed25519 signature, exact
issuer/audience, and event shape. Invalid tokens return 400; temporary backend
failures return 503 so Auth's durable delivery worker retries.
