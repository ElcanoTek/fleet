# ADR-0089: An approval may narrow one declared list argument to a subset of what was staged

- **Status:** Accepted
- **Date:** 2026-10-20
- **Deciders:** fleet maintainers
- **Relates to:** [ADR-0082](0082-approval-card-describers-run-a-declared-read-at-staging.md)
  (approval-card describers), [ADR-0084](0084-a-settled-card-reads-its-outcome-from-the-calls-own-result.md)
  (result cards), the approval-card lifecycle in
  [`APPROVAL-CARDS.md`](../APPROVAL-CARDS.md), the "Governance is one core"
  invariant in `AGENTS.md`.
- **Design note:** [`APPROVAL-NARROWING.md`](../APPROVAL-NARROWING.md)

## Context

Until now an approval approved exactly the staged call: `executeClaimedApproval`
ran the frozen arguments the card showed, byte for byte, or nothing. One
critical call can cover many records, though. A deal plan stages one
`<ssp>_execute_plan(deal_ids=[…])` per system, and that one call creates or
changes every deal in the list. A trader who wanted to leave one deal out had
to decline, ask the agent to re-stage without it, and approve a second card;
in practice they ticked deals on a separate review card and then approved
again. The designer's states 2 to 2d ("uncheck any deal / any change to leave
it out", "Left out" rows, "Approve all (k)") and the owner ask for review and
approval in one step.

That means fleet would execute something other than the staged arguments.
The staged arguments are the record of consent, so the change needs a rule
for what else an approval may run.

## Decision

A bundle may declare, per critical suffix, ONE top-level list argument a person
may narrow on its card (`agent_policy.critical_tool_narrowable_args`, e.g.
`execute_plan: {arg: deal_ids}`). For such a call:

1. **Only that argument, only smaller.** On approve, the person may name a
   subset of the staged values (`approved_values`). Fleet checks, before any
   claim, that the staged value is a list of 2 to 500 distinct non-empty
   strings of at most 200 characters and that the named values are a non-empty subset of it, each named
   once. The call then runs with that argument replaced by the named values in
   their staged order, and with every other argument exactly as staged. A
   narrowing can only leave values out: it cannot add, repeat or reorder one,
   and it cannot touch any other argument. Naming every staged value is an
   ordinary whole approval.
2. **Approve once.** A narrowed approval is a decision about this call only.
   It is refused with a decline, and with any scope other than `once` (it can
   never become a session policy).
3. **Recorded in the claim.** What was narrowed is written in the same UPDATE
   that approves the row (`approvals.narrowed_json`: the argument, every
   staged value, the approved ones). `args_json` stays the staged call. So the
   audit trail holds both what was staged and what was approved, and no row can
   be approved narrowed without the record. Each narrowed approval also logs an
   `audit:` line naming the counts and the left-out staged positions (never the values).
4. **Re-checked at execution.** Before the call runs, fleet re-checks the
   record against the staged call (the argument still holds exactly the
   recorded staged values; the approved values are a non-empty subset in
   staged order). A record that does not match refuses the run. Running the
   staged list whole instead would run values the person left out.
5. **The server is told, and still decides.** The narrowed call carries
   `_meta["fleet/narrowed"] = {"arg", "staged"}` (MCP request metadata, not an
   argument), so a server can check its other arguments (a summary written for
   the staged list, say) against what the person reviewed. The bundle's tool
   re-validates the call as it does any call, and may refuse it.
6. **The model is told.** Fleet adds one labelled line after the tool's result
   in the conversation: what was approved and which values were left out, so
   the agent neither reports them as done nor runs them again unasked.
7. **The person saw the rows.** The web offers a checkbox only on a row of the
   bundle describer's card (ADR-0082) whose `key` names a staged value. The
   server does not require it (a narrowing can only shrink the call), but the
   client never offers to leave out a value it does not show.

Absent the key, nothing changes: no card offers a checkbox, every request
carrying `approved_values` is refused, and every approval runs the staged call
exactly as before.

## Consequences

- The invariant becomes: an approval runs the staged call, **or** that call
  with the one declared list argument narrowed to a recorded non-empty subset
  the person chose on the card. Nothing else about a staged call can change
  between the card and the run.
- One approval can now run fewer records than its card listed. The card, its
  outcome line ("Approved 2 of 3 · 1 left out"), the result breadcrumb and the
  audit line all say so; the settled card marks the left-out rows.
- A bundle whose tool binds its other arguments to the list (a hash or a
  summary of the staged list) must accept a narrowed call by reading
  `_meta["fleet/narrowed"]`, or it will refuse every narrowed call. That is
  the safe failure: the call does not run.
- The grouped card (GROUPED-APPROVALS.md) carries a per-call narrowing in the
  same one request; a call with every row unticked is declined.
- Only one argument per tool can be narrowed. A tool that needs two coupled
  lists narrowed together cannot use this.

## Alternatives considered

- **Re-stage a new card.** Unticking would ask the agent for a new call with
  the smaller list, approved on a new card. That is today's two-step flow the
  owner asked to remove, and it costs a model turn per change of mind.
- **Let the person edit arguments freely** (as the schedule card's edits do).
  Rejected: an arbitrary edit could add a record nobody reviewed or change a
  price. A subset of a declared list is the narrowest edit that serves the
  need, and it can be checked mechanically.
- **Rewrite the dependent arguments too** (recompute a summary for the
  subset). Rejected: fleet would have to know the bundle's argument semantics.
  Telling the server the staged list in `_meta` keeps fleet generic and leaves
  the check to the code that owns it.
