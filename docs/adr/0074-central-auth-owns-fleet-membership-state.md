# ADR-0074: Central Auth owns Fleet membership desired state

- **Status:** Accepted
- **Date:** 2026-09-23
- **Deciders:** Fleet maintainers, Elcano Auth owner

## Context

Auth already gated issuance of Fleet authorization codes, but Fleet separately
owned Chat and Operations Center allowlists. An administrator could create an
account and grant Fleet in Auth while the person still received Fleet's
not-a-member response. Removing the Auth grant likewise ended sessions without
providing a durable way to converge both Fleet databases.

The two Fleet planes cannot share a transaction: Chat and scheduler identities
live in different PostgreSQL databases. Scheduler tasks also reference a stable
user UUID, so deleting its account on revoke would detach task ownership and
make a later grant a different identity.

## Decision

**Auth is the source of desired enabled state and selected Chat/Ops roles for
centrally managed Fleet identities; Fleet validates and enforces that state
locally.**

- Auth sends signed `access+jwt` desired-state events with a monotonic version.
- Fleet durably rejects stale versions and reconciles both databases before
  acknowledging delivery.
- Revocation disables rather than deletes. It invalidates live sessions while
  preserving identities, roles, conversations, tasks, and ownership.
- A later grant re-enables the preserved identities. It never revives a session
  minted before revocation.
- Fleet Admin is a coherent cross-plane role (`admin`/`admin`); split admin
  states from this protocol fail closed.

## Consequences

- Creating an account and granting Fleet in Auth is sufficient; a second Fleet
  allowlist edit is no longer part of normal operations.
- Cross-database application is eventually consistent. A partial failure
  returns a retryable error, and the same version safely reconciles again.
- Existing local/operator controls remain recovery tools, but the next Auth
  event may overwrite enabled state or roles for a centrally managed identity.
- Deployments must update Fleet before Auth begins sending access events.
