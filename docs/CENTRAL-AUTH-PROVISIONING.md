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
team as a third key, `team` (`""` for none). Fleet accepts exactly
`{chat_role, ops_role}` or `{chat_role, ops_role, team}`; any other key fails
closed. A team is at most 64 bytes with no control characters.

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
subject. The same or an older version is ignored. The Operations Center uses a
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
when Auth sends one, the team.
This change does not delete dormant accounts or migrate application data into
Auth.

## The other direction

Role changes, team changes and removals made in Fleet reach Auth through
Fleet's generic
account-events feed ([ACCOUNT-EVENTS.md](ACCOUNT-EVENTS.md),
[ADR-0076](adr/0076-fleet-publishes-account-events.md)) when the operator
points it at Auth. Fleet does not know the receiver is Auth. With the feed
configured, the most recent change on either side wins; without it, the
paragraph above still describes the behavior.
