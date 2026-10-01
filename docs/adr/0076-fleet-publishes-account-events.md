# ADR-0076: Fleet publishes account events; the most recent change wins

- **Status:** Accepted
- **Date:** 2026-09-28
- **Deciders:** Fleet maintainers, Elcano Auth owner
- **Amends:** [ADR-0074](0074-central-auth-owns-fleet-membership-state.md).
  Central Auth is no longer the only writer of a centrally managed identity's
  roles: a change made in Fleet is published and Auth adopts it, so the most
  recent change on either side wins.

## Context

ADR-0074 made Auth the source of desired state for centrally managed Fleet
identities. Fleet's own admin surfaces stayed available, but a role changed in
Fleet was invisible to Auth and was silently overwritten the next time an Auth
administrator touched that person's Fleet settings.

Fleet is also sold on its own. Anything that makes Fleet call, import or
require Auth would break that: a standalone deployment has no Auth, and an
enterprise one may use a different identity provider.

## Decision

**Fleet publishes a generic, signed, durable feed of Chat membership and role
changes to one operator-configured URL; it names no receiver and depends on
none.** Auth is one subscriber (docs/ACCOUNT-EVENTS.md).

- Each event carries the account's full resulting state across the Chat and
  Operations Center databases, read back after the change, and is queued only
  when that state changed.
- Events are HMAC-signed with the existing webhook scheme, delivered in order
  per account from a durable outbox, and retried for 7 days.
- A change Fleet applied on the identity provider's word is published with
  `source: "identity_provider"`, so the provider can ignore its own echo.
- The feed is off unless configured. A URL without a signing secret, or with
  the same secret as the task webhook (`FLEET_WEBHOOK_SECRET` — the schemes
  are identical, so one key would let a task-webhook body verify as an account
  event), refuses to boot; the notifications admin panel refuses to save that
  collision, and switches off a task webhook whose saved secret has it.
- **Fleet holds its own side of "most recent wins".** A change made in Fleet
  (the admin UI, the CLI, the boot seed — every source but
  `identity_provider`) is also written into the provider's stored desired
  state for that email (`external_access_state`: its roles, or `allowed =
  false` for a deletion; never its version). The provisioning push reconciles
  the Ops plane from that row even for a version it has already applied (so a
  retried push can finish a failed Ops write), and without the adoption the
  provider's ordinary at-least-once redelivery of an already-applied version
  would silently put back the role Fleet's admin just changed — published as
  `identity_provider`, the one source the provider may ignore. The provider's
  next real change carries a newer version and still wins, and one that
  commits while Fleet's change is in flight wins too: the adoption locks the
  provider's rows and applies only if they are unchanged since before Fleet's
  change.
- **A deletion is reported only when no access is left.** A Chat account gone
  while its Operations Center identity remains is published as that remaining
  access (`enabled: false`, empty `chat_role`, the Ops role held), never as
  `user.deleted`.
- **Resync repairs deletions, not only live accounts.** `fleet account-events
  resync` also queues `user.deleted` for every email the feed knows once had a
  Chat account that is gone (a provider desired-state row, or an outbox row
  whose latest event is not a deletion), so a lost deletion does not leave the
  provider holding a grant its next push would turn back into an account; it
  adopts the deletion (`allowed = false`) as well.

For centrally managed identities, the rule becomes: **the most recent change
wins, whichever side made it.** Auth decides how to adopt reports (it mirrors
role changes and removals for accounts it has already granted Fleet, and never
creates accounts or grants from a report); Fleet's side of the contract is
only the feed.

## Consequences

- Fleet stays standalone: with no receiver configured nothing changes, and any
  receiver (an audit sink, another identity provider) can use the same feed.
- A role change in Fleet no longer disappears on the next Auth edit, provided
  the receiver adopts it.
- Two writers can race. Ordering between a Fleet change and an Auth change
  made seconds apart depends on the receiver comparing timestamps from two
  hosts; that is acceptable for admin edits and is the receiver's concern.
- The event is queued after the change commits, not in the same transaction
  (the two planes live in different databases), so a crash between them loses
  one event until the next change or `fleet account-events resync`. The same
  window applies to the baseline adoption above: a crash between the change
  and the adoption leaves a redelivered push able to revert that one change,
  until the next change or a push with a newer version.
- The adoption only happens with the feed on. With it off, Fleet has no way to
  tell a provider about its own changes, and ADR-0074 holds unamended: the
  provider's desired state is authoritative, redeliveries included.
- ADR-0074's other decisions stand: Auth events are versioned and applied
  idempotently, revocation disables rather than deletes, and Fleet Admin is
  one coherent cross-plane role.

## Amendment: teams (2026-09-30)

The team is now part of the synced state, in both directions, under the same
rules as the roles:

1. **The feed carries `user.team`.** It is part of the state, so a team-only
   change publishes, a team rename publishes one event per member it moved,
   and a person moving themselves between teams publishes with themselves as
   actor.
2. **The provider may send a team.** The provisioning payload may carry a
   third settings key, `team`. Present, Fleet moves the account to it through
   the same unsharing an admin move does (the person's shares with the old
   team end, exactly as ADR-0057 describes for a leaver). Absent, the team is
   left alone, so an older provider or one with team sync off never touches
   it. A revoke never changes the team. Any key other than `chat_role`,
   `ops_role` and `team` still fails closed.
3. **Adoption covers the team only where the provider manages it.**
   `external_access_state.team` is NULL when the provider never sent one; a
   Fleet-side team change is adopted into a row whose team is set, and never
   starts a provider managing a field it does not know about.
4. **Case is not a change on the provider's word.** Team gates match exactly,
   so a provider team that differs from Fleet's only in case is left as it
   is; rewriting the case would silently detach the person from their team's
   shared projects.

Consequence: moving someone between teams in the provider now changes what
that person shares in Fleet. That is the intended meaning of a team move, and
the reason the provider's side (Central Auth) keeps team sync off until an
operator has imported Fleet's teams into it.
