# ADR-0095: A Fleet that signs in through central Auth has no passwords or account management of its own

- **Status:** Accepted
- **Date:** 2026-10-28
- **Deciders:** Fleet maintainers, Elcano Auth owner
- **Amends:** [ADR-0076](0076-fleet-publishes-account-events.md) for
  deployments that sign in through central Auth: membership flows one way,
  from Auth to Fleet.
- **Relates to:** [ADR-0074](0074-central-auth-owns-fleet-membership-state.md),
  [ADR-0064](0064-application-session-lifetimes.md)
- **Design note:** [`features/central-auth-integration.md`](../features/central-auth-integration.md)

## Context

Fleet kept its own email and password sign-in beside central Auth SSO, as a
break-glass path for when Auth is down. On a client deployment that left a
second set of credentials outside Auth's control: Auth's password policy,
two-factor requirement and account lifecycle did not apply to them, a Fleet
administrator could create password accounts Auth never saw, and the
Operations Center minted its own seven-day bearer from the same passwords.
Since ADR-0074, Auth already owns who may enter a centrally managed Fleet and
with which roles, so the Fleet passwords were the remaining way around it.

## Decision

**Configuring central Auth SSO turns Fleet's own passwords off.** There is no
separate setting: the three `FLEET_OIDC_*` values that make Fleet offer SSO
(issuer, client id, client secret) also make it SSO-only.
`FLEET_OIDC_ALLOW_PASSWORD=1` is the break-glass that turns passwords back on.

- The decision is made once, in `config.Load`, from configuration alone, and
  handed to the browser gateway, chat-server and the Operations Center. It
  never depends on whether Auth answers, so an Auth outage cannot reopen
  passwords.
- Every password path refuses: the gateway's password form, chat-server's
  `/auth/verify`, and the Operations Center's `POST /auth/login` (including
  its public `/v1` alias).
- Credentials already issued from a password stop working: a password session
  cookie (with or without its source claim) and an Operations Center bearer
  minted by its login, at every place either is accepted.
- Auth also manages accounts, roles and teams (`AccountsManagedByAuth`,
  configured SSO alone, so the break-glass does not reopen it). Settings →
  Admin → Users becomes the view-only usage list and Settings → Team shows a
  person's team read-only; chat-server refuses account creation, role and
  team edits, deletion, password resets, team renames and self-serve team
  changes, and the Operations Center's admin-key `POST /users` refuses too.
  Auth's team sync is therefore part of the setup: Fleet no longer changes
  teams, so Auth must send them. Erasing a person's Fleet data stays a root
  command on the server (`fleet admin rm`); Auth can disable access, never
  delete data.
- **One way.** Auth is the single source of truth and pushes desired state
  to Fleet; Fleet does not report account changes back (the account-events
  feed is not pointed at Auth), so there is no "most recent change on either
  side wins" (ADR-0076) on such a deployment, no echo to suppress, and no
  path for an application to change identity data in Auth. A change root
  makes on the box with the CLI is local and Auth's next push for that
  person overwrites it. Bringing an existing Fleet's teams into Auth is a
  one-time, deliberate import, not ongoing sync.
- Not affected: central SSO sessions, the legacy `elcano_auth` cookie, machine
  API keys (bootstrap admin key and typed keys), webhook signatures, the
  operator's shared-token channel and the root CLI.

## Consequences

- At the first update that carries this, every Fleet with central Auth
  configured becomes SSO-only. Anyone who signs in with a Fleet password and
  has no Auth account under the same email loses access until one is created;
  their Fleet data is untouched and is theirs again at their first SSO sign-in.
  An operator who needs time sets `FLEET_OIDC_ALLOW_PASSWORD=1` before
  updating.
- When Auth is down nobody can sign in to the web UI. Root recovers with
  `FLEET_OIDC_ALLOW_PASSWORD=1` and a restart (and `fleet admin add` for a new
  password if needed), then removes it again.
- Password rows stay in both databases. They are inert while passwords are
  off and work again if the break-glass is used.
- The switch is read at startup; changing it needs a restart. A config reload
  never applies it, and reports the change when it sees the decision move.
