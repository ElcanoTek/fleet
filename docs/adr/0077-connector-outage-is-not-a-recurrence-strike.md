# ADR-0077: A connector-outage dead-letter is not a strike against its recurrence

- **Status:** Accepted
- **Date:** 2026-10-05
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0070](0070-dead-lettered-recurrences-spawn-successor.md). A
  dead-letter caused by a declared connector that failed to connect does not
  count toward the two-consecutive-dead-letters park, in either position — up
  to three consecutive such dead-letters, which park the chain on their own.
- **Revised:** 2026-10-05, in review of the same PR. The exemption is bounded,
  the transient classification no longer takes bare "retry"/"unavailable" or
  an HTTP 501, and the in-run retry skips timeouts and has a run-wide budget.

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

1. **Retry the connect, then re-run the occurrence.** A connect failure is
   transient when it is a DNS failure, a timeout, a refused, reset or
   unreachable connection, an HTTP 500/502/503/504/429, or a JSON-RPC error
   that says the condition is temporary (not one that says "do not retry";
   bare "retry" or "unavailable" do not count). A scheduled run retries a
   fast transient failure — every one but a timeout, which already spent the
   whole request timeout — up to three times, a few seconds apart, within one
   15 s allowance per run. When the requirements check still fails only
   because of servers that failed to connect transiently, the error wraps
   `agentcore.ErrConnectorUnavailable` (failure class
   `connector_unavailable`). The runner re-runs the same occurrence about 5
   and then about 10 minutes later on its own budget (`infra_retry_count`,
   migration 074), never the task's `max_retries`.
2. **Record the outage on the dead-letter.** Once that budget is spent the class
   follows the task's `retry_policy` (by default: dead-letter). The dead-letter
   carries `run_outcome = 'connector_unavailable'` and, in
   `run_outcome_detail`, the failed connectors with their causes.
3. **The breaker skips it, up to a bound.** `deadLetterParkReason` does not
   park on such a dead-letter, and `predecessorIsDeadLettered` does not count
   a predecessor that carries it — until the chain has
   `connectorOutageParkStreak` (3) consecutive outage dead-letters. The third
   parks the chain with a reason that names the connector and the streak:
   `A required connector has been unreachable for 3 consecutive occurrences:
   pages (DNS lookup failed (no such host)). …`. Every other dead-letter
   counts exactly as before, and ADR-0073's first-strike park for a malformed
   declaration is unchanged.

## Consequences

- A connector that stays down dead-letters one occurrence per tick, each
  after two re-runs, and the third in a row parks the schedule (nine failed
  attempts across three ticks is not weather). DNS "no such host" stays
  transient, because the production blip on 2026-10-02 was exactly that and
  cleared within minutes; the bound, not the classifier, is what stops a
  typo'd or decommissioned host from dead-lettering forever. Each dead-letter
  notifies, its reason names the server and the connect error, and the park
  notification says the schedule stopped and why. A chain with fewer than
  three outages in a row resumes on its own the first tick the connector
  answers.
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
(`TestConnectorOutageDeadLettersDoNotParkTheChain`,
`TestConnectorOutageParksAfterThreeConsecutiveOccurrences`),
`internal/mcp/transient_test.go`,
`internal/runner/refresh_reliability_test.go` and
`internal/scheduledrun/requirements_outage_test.go`.
