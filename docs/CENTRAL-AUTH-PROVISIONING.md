# Central Auth membership provisioning

## Shipped behavior

Fleet consumes Auth's signed, versioned application-access desired state at
the existing public back-channel endpoint. Granting Fleet in Auth creates or
enables the matching Fleet Chat account and applies the selected Chat role;
it also creates, enables, disables, or re-roles the matching Operations Center
identity. Revoking Fleet disables both planes and ends current sessions without
deleting either identity, its roles, conversations, tasks, or ownership.

Auth's Fleet permission payload is deliberately small and strictly validated:

| Auth control | Fleet value |
| --- | --- |
| Chat Viewer | `viewer` |
| Chat Contributor (default) | `member` |
| Ops None (default) | disabled Ops identity |
| Ops Viewer | `readonly` |
| Ops Contributor | `client` |
| Fleet Admin | Chat `admin` and Ops `admin` |

With team sync switched on in Auth, the payload also carries the account's
team as a third key, `team` (`""` for none), and a newer Auth adds
`team_rename` beside it ([Team renames](#team-renames)). Fleet accepts exactly
`{chat_role, ops_role}`, `{chat_role, ops_role, team}` or `{chat_role,
ops_role, team, team_rename}`; any other key fails closed. A team is at most
64 bytes with no control characters.

- **`team` present:** Fleet moves the account to that team with the same
  unsharing an admin move does. Chats the person shared with their old team
  stop being shared, and they lose the old team's shared projects they do not
  own. A label that differs from the current one only in case is left as it
  is, because team gates match exactly.
- **`team` absent** (an older Auth, a migration backfill, team sync off): the
  team is left alone.
- **A revoke** never changes the team.
- Fleet stores the team the provider last sent (or nothing, when it does not
  manage the team) with the rest of its desired state.

Fleet Admin is one unified grant. In Settings → Admin → Users, selecting it
shows Chat Contributor and Ops Contributor as the effective highlighted
choices. Selecting the already-selected Fleet Admin control turns it off and
defaults to Chat Contributor plus Ops None; either narrower role can then be
changed before saving.

## Delivery and ordering

Auth posts exactly one `access_token` form field to
`/api/auth/backchannel-logout`. The Next.js tier verifies Ed25519 signature,
key ID, issuer, audience, lifetime, event namespace, normalized identity,
action, version, and bounded string settings before forwarding the event over
Fleet's shared-token loopback channel.

The Chat store atomically records the last accepted version per issuer and
subject. The same or an older version is ignored. Events for one identity are
serialized by a transaction-scoped lock on (issuer, subject), taken before the
stored version is read, so this holds for the identity's FIRST events too:
before the first one commits there is no stored row to lock, and two
concurrent first events could otherwise both pass the check and commit in the
wrong order (a revoke overwritten by the older grant). The stored-row upsert
also refuses to lower the version. The Operations Center uses a
separate database, so the handler re-reads durable desired state after each Ops
write and reconciles a concurrently committed newer version before it
acknowledges the request. A retry of the same version still reconciles Ops if a
previous cross-database write failed.

Deploy Fleet's receiver before the Auth sender. Auth retains and retries its
latest desired state until Fleet returns success, so an offline receiver
converges after recovery.

## Security and data retention

- The public endpoint accepts exactly one signed logout or access token and is
  body-size bounded. It does not trust roles sent by a browser.
- Chat revocation rotates both password and external-session epochs.
- Ops revocation clears its scheduler session token. Re-grant therefore cannot
  revive a pre-revocation session.
- Ops disable preserves the scheduler UUID so task ownership and history remain
  attached to the same identity.
- A newly provisioned central-only Chat account has an intentionally invalid
  password digest; a new Ops identity has a random unusable password hash.
- Admin must be coherent across both planes. Split `admin` payloads are
  rejected, and unknown settings, roles or an invalid team fail closed.

## Deliberate scope

Auth controls whether its account may enter Fleet and transports Fleet's
chosen roles. Fleet remains the enforcement and data owner. Existing Fleet
admin APIs and operator commands remain available, but a later Auth desired
state for that identity is authoritative for enabled state, the two roles and,
when Auth sends one, the team. A Fleet with central Auth configured goes
further: it has no passwords of its own and its account pages are view-only,
so people, roles and teams change only in Auth, with team sync on
([central-auth-integration.md](features/central-auth-integration.md#sso-only-auth-signs-people-in-and-manages-them),
ADR-0095). The root CLI still works there.
This change does not delete dormant accounts or migrate application data into
Auth.

## Team renames

Renaming a team in Auth is one app-wide message, not a push per person.
Pushing each member's new team would read as moving them: Fleet would
unshare what they shared with the old team and leave its shared projects
under the old name. So Auth signs a `team+jwt` (no `sub` or `email`; one
event, `urn:elcanotek:event:team-rename`, carrying exactly `from`, `to` and a
`version` that rises by one per rename) and posts it as the single form field
`team_token` to the same back-channel endpoint. The web tier verifies it like
the other two (signature, issuer, audience, `exp`, no account claims) and
forwards it over the shared-secret channel to chat-server's
`/auth/external-team-rename`, which applies the same share-keeping rename an
admin's Rename team does (ADR-0093) and records the version in the same
transaction (`external_team_renames`, one row per issuer). A version at or
below the recorded one is a no-op, so a redelivery after a lost
acknowledgement changes nothing; a rename of a team nobody in Fleet is in is
recorded as done. 204 means applied or already applied, 400 malformed, 503
retry.

Ordering is mostly Auth's job, and it is what keeps sharing intact: Auth
sends its renames for Fleet in version order, one at a time, holds every
per-person push for Fleet while one is undelivered (so Fleet never sees the
new name before the rename), and rewrites any per-person push still queued
with the old name to the new one (so Fleet never sees the old name after it).
Fleet's per-person path compares a pushed team with the person's current
team, so the pushes that follow a rename change nothing.

Fleet adds one guard of its own, for a push that arrives late (Auth gave up
waiting on it while Fleet was still working on it). Each team push carries
`team_rename`, the version of Auth's newest rename when it was written. Under
the same per-issuer lock a rename takes (shared here, exclusive there), Fleet
ignores the team in a push whose `team_rename` is below the last rename it
applied; the roles still apply. So the settings Fleet accepts are
`{chat_role, ops_role}`, plus `team`, plus `team` and `team_rename` (a
non-negative integer, only next to a team); anything else fails closed.

## One way only

Membership flows from Auth to Fleet and not back
([ADR-0095](adr/0095-central-auth-fleets-have-no-passwords.md)). A Fleet that
signs in through Auth changes no account, role or team itself, and its
account-events feed ([ACCOUNT-EVENTS.md](ACCOUNT-EVENTS.md)) is not pointed at
Auth. ADR-0076's "most recent change on either side wins" applied before
that; it no longer does for these deployments.
