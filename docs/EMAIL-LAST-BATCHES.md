# Email-last batches

A batch run creates or updates N records, then sends ONE summary email that
lists every outcome. Intake apps then post the results callback. Fleet's audit
gate (ADR-0034) had no notion of that shape, so one create that failed for
good wedged the run. The only way out was an abort, and the abort then blocked
the email, so the run ended in silence. This note records the port of the v1
engine's fixes (cutlass #749.3, a4b76d83, #1087). The decision is
[ADR-0081](adr/0081-email-last-batches-settle-definitive-create-failures.md).

## Bundle opt-in

```yaml
agent_policy:
  critical_tools: [execute_deal_from_prompt_inputs, create_deal, update_deal]
  email_last_tools: [update_deal]
  settleable_create_tools: [execute_deal_from_prompt_inputs, create_deal]
```

Every member must also be in `critical_tools`, and no member may be an email
tool. `fleet validate-config` reports a violation as `agent_policy=fail`, and
boot logs it and ignores the member. Settleable members are email-last
members too. So is every critical suffix that can carry the same write as a
member: its `critical_tool_aliases` twins and the `critical_tool_substitutes`
targets listed under it (closed to a fixpoint at boot), since the audit gate
lets those names authorize and discharge the member's commitments. Neither
list set means no change.

## What shipped (`internal/agentcore/audit_email_last.go`)

| Rule | Where |
|---|---|
| `send_email` / `send_template_email` refused while an email-last obligation is unsettled; a successful one of either is the summary email | `checkSummaryEmailOrder`, `noteTemplateEmailResult` |
| email-last calls refused after the summary email | `checkEmailLastOrder` |
| a definitive create failure settles one unit; an ambiguous one never does | `noteCreateFailure` |
| after the email, settled-failed units no longer block finish | `finishOwed` |
| abort: one `send_email` passes without an audit; finish nudged ≤ 3 times until sent | `abortEmailAllowed`, `abortNotifyFinishNudge` |
| typed `deal_name` binds a create unit to one record; prepared handles map back | `commitmentAuthorizes`, `markTypedExecuted`, `recordPreparedDeal` |
| re-audit restates unbound creates by count / name | `restateUnboundEntry` |

A **definitive** failure means the tool ran, its result reported failure
(`success: false` or a top-level `error`), and no ambiguous marker appears
anywhere in the result. The markers are `ambiguous_transport`,
`outcome_unknown`, `deal_already_created`, `write_state: "unknown"`,
`written: true`, `deal_created: true`, a `*_create_call_failed` code without
4xx/rejection evidence, and fleet's MCP transport texts. A transport error,
an is-error result or a malformed per-record `results[]` never settles, and
puts the record's settled unit back.

A run becomes email-last work when it declares an email-last tool (typed or
legacy free-text) or attempts one. A call blocked before the audit and then
completed, or definitively failed, through a same-server alias or substitute
is retired, so it cannot hold the email back. The duplicate-send guard runs
after the email-last ordering, and a payload first delivered before the batch
work began is never "already done": it cannot stand as the summary email,
which must be sent fresh from the final outcomes.

Attempts tie to units by record identity, which is the call's
`name` / `deal_name` / `display_name` (top level, `payload`, `payload.deal`,
`deal` or `overrides`), else its `prepared_deal_id`'s prepare-step name, else
its record id. A unit declared with `deal_name` is ridden only by a call
whose readable name is exactly that name, whichever member of the tool family
runs it; a call with no readable name (no name key, or a `prepared_deal_id` no
prepare step returned) is refused against it, never let through on trust.
A corrected retry discharges its own record's settled unit.
Another record's success prefers an unsettled unit, so it never makes a
never-attempted record look settled.

## Deviations from v1

- **Bundle-declared.** v1 hardcoded the create suffixes and treated every
  non-email/presentation critical tool as deal work. Fleet takes both lists
  from the bundle.
- **Generic `*_create_call_failed`.** v1 listed four servers. Fleet matches the
  code shape, so a server whose answered non-4xx failure v1 settled stays
  unsettled here. That is the safe direction.
- **"Summary email sent"** means an email that succeeded *after* email-last
  work began. v1 counted any email, since its cap was one.
- **Restate is scoped to settleable creates.** Every other tool keeps fleet's
  unbound same-tool supersede (FIX 7) and the #1535 refusal correction. A
  name-bound refusal is not recorded for #1535. Otherwise a re-audit naming one
  record could retire every named unit the refused call collided with.
- **Prepared handles.** Any successful call that named a record and returned
  `prepared_deal_id` maps the handle. v1 keyed this on prepare-tool suffixes.

## Deferred

- The bundle (elcano-config) must add the two lists after this release ships.
  Until then nothing changes in production.
- The scheduled driver's end-of-run verifier does not know about settled-failed
  creates. It may spend a repair round, whose write is refused as post-email.
- A rename-correction re-audit (a named unit refused a call under a new name)
  adds the new name. It does not retire the old unit, which stays owed until
  the run aborts. This matches v1.
- An under-count case: a re-audit that declares only what is left, plus one
  new record, adds nothing. The extra create is refused until the ledger
  empties. This fails safe and matches v1.
