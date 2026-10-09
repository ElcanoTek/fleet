# ADR-0081: Email-last batches settle definitive create failures and still notify on abort

- **Status:** Accepted
- **Date:** 2026-10-09
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0034](0034-audit-gate-commitment-binding.md). For tools a
  bundle opts in, an outstanding create commitment whose latest attempt failed
  definitively no longer blocks the summary email, nor finish once that email
  has gone out. An abort no longer silences the run's single summary email.
- **Design note:** [`EMAIL-LAST-BATCHES.md`](../EMAIL-LAST-BATCHES.md)

## Context

ADR-0034 refuses finish while any declared commitment is outstanding, and only
a SUCCESS discharges one. A batch-create run (N record creates, then ONE
summary email listing every outcome, then a results callback) therefore had a
single exit when one create failed for a reason no argument change can fix:
`confirm_audit(success=false)`. The abort cleared the audit token, so the
summary email was then refused as "requires audit first", and finish was
allowed with no email at all. The reader got silence for a batch that was
mostly live, and the results callback that follows the email never ran. The
v1 engine hit this in production on 2026-10-05 (cutlass #1087) and had already
fixed the abort half (cutlass #749.3) and the email ordering (a4b76d83, the
2026-07-20 stale-report incident).

## Decision

A bundle may name, in `agent_policy`:

- `email_last_tools`: critical suffixes whose run reports by one summary email
  sent last; and
- `settleable_create_tools`: record-create suffixes (implicitly email-last too)
  whose UNBOUND commitment is settled by a definitive failure.

Once a run declares or attempts an email-last tool:

1. `send_email` is refused while any email-last obligation is unsettled
   (typed or legacy commitment, or a blocked-before-audit call).
2. Every email-last call is refused after the summary email has gone out.
3. A settleable create whose latest attempt failed **definitively** — the tool
   ran and reported failure, with no ambiguous-outcome marker — is
   settled-failed: it stays outstanding (a corrected retry still discharges it)
   but releases the email and, after the email, finish. An **ambiguous**
   outcome (an `*_ambiguous_transport` / `*_outcome_unknown` /
   `deal_already_created` / `write_state: "unknown"` marker, an unproven
   `*_create_call_failed`, a transport error, an is-error result, fleet's own
   "request not delivered" / "outcome is UNKNOWN" MCP texts) never settles,
   and puts a settled unit back.
4. After `confirm_audit(success=false)`, exactly one `send_email` passes with
   no live audit, and finish is refused (at most three times) until it is
   sent. The abort stays the run's verdict.
5. A typed entry may carry `deal_name`, binding the create unit to one record;
   a re-audit restates unbound create entries by count (or name) instead of
   stacking or replacing them.

Nothing changes for a bundle that declares neither list.

## Consequences

- The rule is data: fleet names no SSP and no create tool. A bundle adopts it
  only after the fleet release that understands the fields ships (strict
  decoding refuses unknown keys).
- Settling trusts the server's failure payload. The markers are the
  cutlass-family wire contract; a server that reports an unknown outcome
  without one would settle wrongly. The match is deliberately broad (any
  `*_create_call_failed` without 4xx evidence is ambiguous) so the error
  direction is "email held back", not "duplicate create".
- The end-of-run verifier (scheduled driver) is unchanged and may still ask
  for a repair round over a settled-failed create; the write is then refused
  as post-email. See the design note's follow-ups.
