# ADR-0075: Fleet publishes account events; the most recent change wins

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
- The feed is off unless configured. A URL without a signing secret refuses
  to boot.

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
  one event until the next change or `fleet account-events resync`.
- ADR-0074's other decisions stand: Auth events are versioned and applied
  idempotently, revocation disables rather than deletes, and Fleet Admin is
  one coherent cross-plane role.
