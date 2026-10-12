# ADR-0084: A settled approval card reads its outcome from the call's own result

- **Status:** Accepted
- **Date:** 2026-10-11
- **Deciders:** fleet maintainers
- **Relates to:** [ADR-0082](0082-approval-card-describers-run-a-declared-read-at-staging.md)
  (approval-card describers), [ADR-0080](0080-generative-ui-cards-are-declarative-data.md)
  (cards are declarative data), the "Governance is one core" invariant in
  `AGENTS.md`.
- **Design note:** [`APPROVAL-RESULT-CARDS.md`](../APPROVAL-RESULT-CARDS.md)

## Context

After a person approves a described card (ADR-0082), the settled card says
"Applied · <title>" above the tool's raw JSON result. The designer's completed
card asks for more: a one-line outcome ("Created 1 Index Exchange deal …
verified"), a row per record with both of its ids, a status and a link, plain
failure states ("Not applied: <reason>", "Applied, but the read-back didn't
match", "Checking whether this deal was created"), the raw JSON collapsed
under Details, and for a plan, "Retry failed (N)". Only the bundle knows what
its result means. Fleet has to get that meaning in a form it can render.

There were two ways to do it:

1. **A result describer.** A second bundle-declared read, mapped like
   ADR-0082's describer (`critical_tool_result_describers:
   {execute_plan: describe_plan_result}`). Once the approved call returns,
   fleet calls it with the staged arguments and the result, and renders what
   it answers.
2. **A result card in the result.** The approved tool's own result carries a
   structured `result_card` object next to its normal fields. Fleet checks it
   against a fixed schema and renders it. No second call is made.

## Decision

Fleet uses option 2. A bundle lists critical suffixes in
`agent_policy.critical_tool_result_cards`. For a matching tool:

1. **The approved call's own result is the only source.** Once the approved
   call has returned and passed the post-execution governance (redaction, PII
   and guardrail screening), fleet looks for a top-level `result_card` object
   in that governed text. It reads the governed text before the size bound, so
   a long batch result is still read whole. Fleet calls no other tool, opens
   no scope, and sends no arguments anywhere. A tool that reported an error,
   or a result that governance blocked, gets no card.
2. **The schema is fixed and strict.** It is text-only. Unknown keys, nulls,
   wrong types, trailing data, control and bidirectional characters, and
   non-https links are refused. The limits are the describer card's. The card
   is read from governed text, so a card that carries the redactor's marker
   (a secret was scrubbed out of it) is refused, and so is one the secret
   redaction would still change.
3. **What the card claims is held to its rows.** The outcome sets the card's
   tone, so it may not contradict the rows. A `done` card may not carry a
   failed, mismatched or unclear row, and a `failed` card may not carry an
   applied one. A retry may name only records of the card that are confirmed
   not applied (`failed`) or never attempted (`not_run`), and never one whose
   outcome is unclear or mismatched. The bundle decides which records qualify.
   Fleet enforces the floor.
4. **Display data on the row.** The canonical card is written once to
   `approvals.result_card_json`, while the row is still executing and just
   before the outcome is recorded. It is served only on a success fleet
   recorded (approved, `is_err` false), and it is re-validated on every read.
   It never changes what ran, what was recorded, or what the model sees: the
   model's tool result is the tool's own text, unchanged.
5. **Retry is a user turn.** "Retry failed (N)" sends a fleet-worded user
   message that names the approved call and the keys, the way **Ask again**
   does. Nothing runs from the click. The agent stages the retry, and it
   needs a new approval.
6. **Notify records too.** A notify-mode record (#1153) is written before its
   call runs. For a member of the list, the loop's result hook
   (`RecordToolResult`) hands the record its call's governed result once
   (`agentcore.ActionResultRecorder`), and the record stores and shows that
   result's card. A record never offers a retry, because a notify tool runs
   without an approval and its retry would too. The hook carries no call id, so two recorded calls with the
   same tool and input get no card at all rather than risk the wrong one. A
   transport without the optional interface records exactly as before.
7. **Any failure keeps today's card.** That covers no `result_card`, a result
   that isn't a JSON object, a refused card, or a failed store write. The
   outcome is never at stake.

### Why not a result describer

- **There is no turn scope after approval.** The approval runs in its own
  request, often long after the turn that staged it, on a fresh per-approval
  scope (`approvalMCPScope`). ADR-0082's guarantees rest on the *turn's own*
  scope and gates: its persona policy, its `pre_tool_use` hooks, and its seat.
  A second call at settlement would need those gates re-created outside the
  loop, or would have to run without them. Option 2 needs neither.
- **One source of truth.** A describer called after the write could read
  state that has changed since, or disagree with the result the model sees.
  A card inside the result comes from the same process, at the same moment,
  as the outcome it describes.
- **Simpler failure modes.** Option 2 adds no latency to the approval path,
  no timeout to budget, no second network failure, and no extra argument
  payload: the result would otherwise be fed back to a tool as its input.
- **No new tool on the bundle.** The bundle adds a field to a result it
  already builds, rather than a ninth tool per SSP server that has to be
  kept parallel-safe and non-critical.

The cost is that the card rides the model-visible result, so the model sees
it too, and that adds tokens. The card is the same facts the result already
carries, in plain words.

## Consequences

- A bundle gets the designer's completed card without fleet knowing its
  domain, and without a second call. Fleet enforces only the schema and the
  outcome/row/retry consistency rules.
- A settled card of a tool that didn't opt in, and every payload without a
  stored card, is byte-identical to before: the `result_card` key is present
  only when a card is.
- The bundle can still describe an outcome wrongly. For example, it can say
  "verified" when it didn't verify. Fleet trusts the bundle's words as it
  trusts its result text, but it can't let a green card carry a failed row,
  and it can't offer a retry of a record whose outcome is unclear.
- The notify-record hook is display only. It runs after the call, on the
  loop's existing result record, and it cannot change the call or its result.
