# ADR-0096: Fleet no longer publishes account events

- **Status:** Accepted
- **Date:** 2026-10-29
- **Deciders:** Fleet maintainers, Elcano Auth owner
- **Supersedes:** [ADR-0076](0076-fleet-publishes-account-events.md)
- **Relates to:** [ADR-0074](0074-central-auth-owns-fleet-membership-state.md),
  [ADR-0095](0095-central-auth-fleets-have-no-passwords.md)

## Context

ADR-0076 gave Fleet a generic, signed, durable feed of Chat membership, role
and team changes (`FLEET_ACCOUNT_EVENTS_URL`), so central Auth could adopt a
change made in Fleet and "the most recent change on either side wins". To
keep that rule on Fleet's side as well, every Fleet-side change was also
written into the identity provider's stored desired state (the baseline
adoption), so a redelivered push could not put the old role back.

ADR-0095 made membership flow one way: a Fleet that signs in through central
Auth leaves accounts, roles and teams to Auth, and its account pages are
view-only. Auth then removed the endpoint that received the feed (auth#108).
Central Auth was the feed's only subscriber. No Fleet Elcano runs has it
configured, and a Fleet that signs in with its own passwords or another
identity provider manages its own accounts and has nothing to report to.

What was left was about 1,400 lines of production code and 1,500 of tests:
the outbox, its deliverer, retries and retention, `fleet account-events
status|resync`, a recorder hooked into every admin, CLI, import, boot-seed
and provisioning write, the baseline adoption with its per-account advisory
lock, config validation, and a rule that the task webhook's signing secret
differ from the feed's.

## Decision

**Fleet no longer publishes account events.** The feed, its recorder hooks,
its deliverer, the baseline adoption, `fleet account-events status` and
`fleet account-events resync`, and the webhook-secret reservation are
removed.

- `fleet account-events export` stays, under its old name: central Auth's
  `auth app import-teams` reads Fleet's existing teams and roles from it
  (production-support's `register-auth-apps.sh` runs it). Its output fields
  (`email`, `enabled`, `chat_role`, `ops_role`, `team`) do not change.
- `FLEET_ACCOUNT_EVENTS_URL` and `FLEET_ACCOUNT_EVENTS_SECRET` are retired.
  A leftover value no longer refuses boot (a URL without a secret used to);
  `config.Load` logs a warning naming each one so the operator removes it.
  Both stay on the list of names kept out of MCP and sandbox environments,
  and `fleet env show` still masks the URL, because systemd passes a stale
  value from the env file to the process anyway.
- The `account_events` table stays, unread and unwritten, until a later
  migration drops it, following [MIGRATIONS.md](../MIGRATIONS.md): stop using
  a table first, drop it in a follow-on release, so a rolled-back binary does
  not meet a missing table.

## Enforcement

- The feed's code is gone: `internal/accountevents`, the outbox functions in
  `internal/store`, the recorder hooks in `internal/httpapi`,
  `internal/admincli` and `cmd/fleet`.
- `TestLoad_RetiredAccountEventsKnobsOnlyWarn` (`internal/config`): a leftover
  URL without a secret loads and is named in a warning that does not print
  it.
- `TestCLIExport` (`internal/admincli`): the export's lines and field names,
  disabled accounts included; `resync` is refused.

## Consequences

- One direction everywhere. Central Auth pushes desired state; Fleet applies
  it and reports nothing.
- A change made in Fleet to an account central Auth provisions is no longer
  written into Auth's stored desired state. Any push for that person, a
  redelivery of a version Fleet already applied included, sets the
  Operations Center role back to Auth's (the push reconciles Ops from the
  stored state every time); the Chat role and team change back only with
  Auth's next version for that person. That is ADR-0095's rule ("Auth's next
  push overwrites it"). On a Fleet that signs in through central Auth the
  account pages already refuse writes, so only root with the CLI can make
  that change. A Fleet with `FLEET_OIDC_GENERIC=1` that still receives Auth's
  provisioning pushes has editable account pages, and there an admin's edit
  meets the same overwrite. Every Elcano Fleet already ran this way, with the
  feed off.
- A standalone or generic-OIDC Fleet that wanted an audit stream of account
  changes has none. If one is needed later, it should be designed for that
  receiver; the removed code is in git history.

## Alternatives considered

- **Keep the feed for other receivers.** Rejected: there are none, and it
  kept the baseline adoption, whose only purpose was two-way sync, wired into
  every account write.
- **Keep the feed but drop the baseline adoption.** Smaller, but still about
  a thousand lines with no subscriber.
- **Rename `fleet account-events export`.** Rejected for now: Auth's import
  documentation and production-support's registration script call it by this
  name.
