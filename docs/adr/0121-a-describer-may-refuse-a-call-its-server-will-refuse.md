# ADR-0121: A describer may refuse to stage a call its server will refuse

- **Status:** Accepted
- **Date:** 2026-11-25
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0082](0082-approval-card-describers-run-a-declared-read-at-staging.md)
  rules 5 ("staging never fails because of it") and 6 ("the output never
  reaches the model").

## Context

A deal plan's `<ssp>_execute_plan` call binds the plan file by its sha256,
which the model copies from the plan's finalize or preview result. On
2026-11-24 the model cut that hash short on two calls. The bundle's
describer saw the problem at staging and returned its "couldn't be checked,
approving it won't run anything" card, but the card was staged anyway, a
person approved both, and both failed with `values_sha256_invalid`. Nothing
was sent, but someone was asked to approve two calls that could never run,
and the model only learned what was wrong after they did.

Fleet already refuses a call before a card exists when it can tell the call
will fail: a task prompt the create seam would refuse
(`prevalidateStagedTaskPrompt`), an MCP argument outside the closed set the
tool's schema declares (`prevalidateStagedMCPArguments`), and an email the
sending server's `validate_email_content` rejects (`prevalidateEmail`). Each
returns a `StageRefusedError`, and the model gets `ARGUMENTS_REFUSED`. Fleet
cannot read a plan file's hash, so only the bundle's describer, which already
runs at staging, knows.

## Decision

A describer card may carry `refuse: {code, message}`. When a describer
returns a card that passes every existing check (strict schema, text rules,
secret-redaction check) and carries a refusal, `Stage` returns a
`StageRefusedError` built from the message and code, before the supersede
and before any approval row exists. Nothing is stored and no approval event
is emitted.

Everything else in ADR-0082 holds. The describer is still a declared read,
called once in the turn's own scope with the model's own arguments, bounded
to five seconds. A timeout, an error or an invalid card still falls back to
the generic card and stages it: only an explicit, valid refusal stops a
card. A refusal can only narrow: it stops a card, and never approves,
changes or runs a call.

The refusal message reaches the model. It is the bundle server's own text
about a call to that same server, which the model would read anyway when the
server refused the call after approval, and it is held to the card's text
and redaction rules first.

Autonomous mode, which runs the call without a card, does not act on a
refusal: the server refuses the call itself and there is no card anyone
could approve by mistake.

## Consequences

- A call the server will refuse no longer asks anyone to approve it, and the
  model learns what to fix in the same turn.
- A bundle decides what counts as certain refusal. It should refuse only on
  problems in the call's own arguments that cannot change before approval;
  anything that might (a stale preview, live SSP state) should stay a flag
  on a staged card.
- An older fleet decodes the card strictly and rejects the unknown `refuse`
  key, so it falls back to the generic card and stages it, as before. A
  bundle can ship refusals before every box has this release.
