# ADR-0077: A connector-outage dead-letter is not a strike against its recurrence

- **Status:** Accepted
- **Date:** 2026-10-05
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0070](0070-dead-lettered-recurrences-spawn-successor.md). A
  dead-letter caused by a declared connector that failed to connect does not
  count toward the two-consecutive-dead-letters park, in either position.

## Context

ADR-0070 parks a recurring chain when two consecutive occurrences
dead-letter: "one bad day continues, two is systemic". It assumes that the
cause of a dead-letter is the job's.

Production (2026-09-24 to 10-04) dead-lettered page refreshes on causes that
belonged to nobody's job. A one-minute DNS blip (`lookup pages.elcanotek.com:
no such host`) and a vendor answering `Auth validation temporarily
unavailable. Retry.` made the run skip the server. The EXECUTION REQUIREMENTS
dispatch check then dead-lettered the task as `non-retryable failure
(terminal)` after 9 seconds. Two such days in a row parked a daily schedule
until a human replayed it, although nothing about the job had to change.

## Decision

1. **Retry the connect, then re-run the occurrence.** A scheduled run retries a
   transient registration failure three times, a few seconds apart. When the
   requirements check still fails only because of servers that failed to
   connect transiently, the error wraps `agentcore.ErrConnectorUnavailable`
   (failure class `connector_unavailable`). The runner re-runs the same
   occurrence about 5 and then about 10 minutes later on its own budget
   (`infra_retry_count`, migration 074), never the task's `max_retries`.
2. **Record the outage on the dead-letter.** Once that budget is spent the class
   follows the task's `retry_policy` (by default: dead-letter). The dead-letter
   carries `run_outcome = 'connector_unavailable'`.
3. **The breaker skips it.** `deadLetterParkReason` never parks on such a
   dead-letter, and `predecessorIsDeadLettered` does not count a predecessor
   that carries it. Every other dead-letter counts exactly as before, and
   ADR-0073's first-strike park for a malformed declaration is unchanged.

## Consequences

- A connector that stays down for days dead-letters one occurrence per day
  without parking the schedule. Each dead-letter still notifies, and its
  reason names the server and the connect error, so the outage is visible.
  The schedule resumes on its own the first day the connector answers.
- The classification is narrow on purpose. A server that is not configured or
  not selected, a tool no connected server provides, and a connect failure
  that is not transient (a refused credential, a bad URL) stay terminal and
  count toward the breaker as before.
- A bare tool name in `required_tools` cannot be traced to a server whose
  catalog was never fetched; it is attributed to the run's transient outage
  when there is one. That can classify a misspelled bare name as an outage
  on a day a selected server is down; the cost is bounded (two re-runs, then a
  dead-letter that names the outage).

## Enforcement

`internal/sched/storage/run_outcome_test.go`
(`TestConnectorOutageDeadLettersDoNotParkTheChain`),
`internal/runner/refresh_reliability_test.go` and
`internal/scheduledrun/requirements_outage_test.go`.
