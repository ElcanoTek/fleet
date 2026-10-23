# ADR-0092: A blocked run notifies as `blocked`, and a failure subscriber hears it

- **Status:** Accepted
- **Date:** 2026-10-23
- **Deciders:** fleet maintainers
- **Relates to:** [CONDITIONAL-TASK-COMPLETION.md](../CONDITIONAL-TASK-COMPLETION.md#a-blocked-outcome-is-visible-completionblocked_when)
  (`completion.blocked_when`, migration 074) and
  [SCHEDULED-REFRESH-RELIABILITY.md](../SCHEDULED-REFRESH-RELIABILITY.md) §C,
  whose "third blocked run in a row notifies as a failure" rule this replaces.

## Context

A run whose declared completion clause records a blocked outcome finishes as
`success` with `run_outcome = 'blocked'`. The Operations Center shows it as an
amber **Blocked** badge, but the notifier (`internal/notify`) only knew
`success`, `failure` and `progress`. A blocked run was sent as a `success`
event whose message began "Blocked: …", and only the third blocked occurrence
in a row of a recurring lineage was escalated to a `failure` event.

So a deployment that notifies only on failures — the common setting, since a
success email per run is noise — heard nothing about the first two blocked
runs of a daily refresh. The people who own those refreshes found stale client
dashboards by hand, days later.

## Decision

1. **`blocked` is a notification status.** `notify.StatusBlocked` is raised by
   the runner for a run that landed `success` with run outcome `blocked`
   (`runner.notifyBlocked`). Its `Event.Message` is the recorded
   `run_outcome_detail`, whitespace-collapsed and length-capped. The task's
   own status stays `success`; only the notification says blocked.
2. **`failure` covers `blocked` in the On filter.** An `On` list (the
   `FLEET_NOTIFY_ON` env var or the admin panel's *Notify on* chips) that
   names `failure` also fires for `blocked`. `blocked` may also be named on
   its own, which does not widen to real failures. `success` never covers it.
   An empty list still means every status, and `always` still means every
   status. The filter applies to every channel alike (email, webhook, Web
   Push).
3. **The third blocked run in a row is still called out, but in words.** The
   run that makes three consecutive blocked occurrences of a recurring lineage
   is sent as a `blocked` event whose message begins `Blocked 3 runs in a
   row — …`. It is no longer escalated to `failure`: a failure subscriber
   now hears every blocked run, so the escalation would only change the
   status a webhook receiver sees for one run in three.
4. **The channels render it.** Email subject `Fleet task blocked: <name>`, a
   body that leads with `Blocked: <name>` and labels the message `Reason`;
   Web Push `⚠ Blocked: <name> (<duration>)` without the reason (a push never
   carries `Message`, for lock-screen privacy); the default webhook body adds
   one key, `"reason"`, for a blocked event only. Every other event renders
   exactly the keys it always had.

## Enforcement

- `internal/notify`: `TestShouldFire_BlockedSemantics`,
  `TestNotify_FailureFilterDeliversBlocked`, `TestRenderEmail_Blocked`,
  `TestRenderWebhookBody_DefaultBlockedEvent` (which also pins that no other
  status gains a key).
- `internal/webpush`: `TestEventTitle_Blocked`.
- `internal/runner`: `TestSingleBlockedRunNotifiesAsBlocked`,
  `TestBlockedRunIsRecordedAndTheThirdInARowAlerts`,
  `TestBlockedMessageIsCollapsedAndCapped`.
- `internal/store`: `TestNotifySettingsNormalizeAcceptsBlocked` (the panel may
  save `blocked`).

## Consequences

- A deployment that listens for failures now gets an email, webhook call and
  push for every blocked run. In the 2026-10 production sample about a quarter
  of refresh runs were blocked, so that can be a real increase in volume. An
  operator who wants failures without blocked runs has no single chip for
  that; the per-owner daily digest ([OWNER-DIGEST.md](../OWNER-DIGEST.md)) is
  the quieter route for owners.
- **Webhook receivers see a new status value**, `blocked`, and a new key,
  `reason`, on those events. A receiver that switches on `status` and treats
  unknown values as success, or rejects unknown keys, needs updating. A
  receiver that paged on the old third-in-a-row `failure` event now gets that
  run as `blocked`, with the streak named in the email message and in
  `reason`.
- A custom webhook template that tested `{{if eq .Status "success"}}` to
  detect a green run now treats a blocked run as not-success, which is the
  intended reading.

## Alternatives considered

- **Keep `success` and rely on the message.** Rejected: the On filter works on
  status, so a failure-only deployment could never hear a blocked run.
- **Send blocked runs as `failure`.** Rejected: the run did not fail (retry,
  dead-letter and recurrence treat it as a success), and receivers could not
  tell a stale dashboard from a broken job.
- **Keep escalating the third blocked run to `failure`.** Rejected (see
  decision 3); it also meant an `On` list of `blocked` alone would miss that
  run.
