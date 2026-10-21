# Approval narrowing: approve some of the records one call covers

Design note for `agent_policy.critical_tool_narrowable_args`: a bundle can
declare, for a critical tool, the one list argument a person may narrow on its
approval card. The readable card's rows that name a staged value get a
checkbox; the person unticks the ones to leave out and approves the rest in one
click. Recorded as
[ADR-0085](adr/0085-an-approval-may-narrow-a-declared-list-argument.md),
because it changes what an approval runs. It builds on
[APPROVAL-CARD-DESCRIBERS.md](APPROVAL-CARD-DESCRIBERS.md) (the rows),
[GROUPED-APPROVALS.md](GROUPED-APPROVALS.md) (one card for several calls) and
[APPROVAL-RESULT-CARDS.md](APPROVAL-RESULT-CARDS.md) (the settled card).

## Why

A deal plan books one call per external system, and each call names every deal
it runs in one list (`<ssp>_execute_plan(deal_ids=[…])`). Grouped approvals let
a person leave out a whole call, but not one deal inside a call. A trader who
wanted to drop one deal ticked deals on a separate review card and then
approved a second card. The designer's states 2, 2b, 2c and 2d ("uncheck any
deal / any change to leave it out", "Left out" rows, "Approve all (k)") put
review and approval on one card.

## What shipped

### The bundle opts in, per tool

```yaml
agent_policy:
  critical_tools: [execute_plan]
  critical_tool_card_describers: {execute_plan: describe_plan_call}
  critical_tool_narrowable_args:
    execute_plan: {arg: deal_ids}
```

- Keys match like `critical_tools`; the longest matching suffix wins, as for
  `critical_tool_card_describers`.
- The key must stage a card for an MCP call: a critical suffix, not `notify`
  mode, not a handler-only card. `arg` must be a plain top-level argument name
  (`[A-Za-z_][A-Za-z0-9_]{0,63}`). Boot drops a bad entry with a log line, and
  `fleet validate-config` reports it in the `agent_policy` floor check
  (`agentcore.NarrowableArgProblems`, the same code).
- Installed at boot from `cmd/fleet/main.go`.
- **Absent key = today's behaviour, exactly.** No card offers a checkbox, no
  payload gains a key, and the approve POST refuses `approved_values`.

### The rows: the describer's `key`

A describer card item may now carry `key`: the staged value of the narrowable
argument the row stands for. It is optional, at most 200 characters, under the
card's text rules, and unique within the card (a repeated key refuses the
card, which then falls back to the generic card, as any refused card does). An
older fleet refuses a card that carries it, so a bundle sends it only after
this release.

Only a row whose key is one of the staged values is selectable. A row without
a key, or whose key names no staged value, is shown as before. A staged value
with no row (a card that cut its list to fit its limits) cannot be left out
from the card.

### The pending card

`narrowable: {arg, values}` rides the live `tool.approval_required` event, each
`pending_approvals` row and the one-card GET, for a declared tool whose staged
value of `arg` is a list of 2 to 500 distinct non-empty strings of at most 200
characters each (a row's `key` must equal the value, and keys are bounded the
same; `stagedNarrowable`). Any other card carries no key.

On the readable card (`GenericActionCard` with `ReadablePendingRecords`):

- each selectable row has a checkbox, ticked, named "Include <label> (<id>)"
  for a screen reader;
- a live count above the rows reads **3 of 3 selected** (`aria-live="polite"`);
- an unticked row greys out and reads **Left out** in place of its change;
- the button reads **Approve** while every row is ticked, **Approve 2 of 3**
  once one is not, and **Decline** when none is; a line under it says "Covers
  only the 2 selected. 1 left out is not sent." or "Nothing is selected, so
  this declines the action.";
- apply-all is not offered on a narrowable card (a narrowed approval is a
  decision about this call only).

A one-record card keeps its row layout when the record is selectable, so the
checkbox is there.

### The decision

The approve POST takes `approved_values`, the ticked values:

```json
{"approved": true, "scope": "once", "approved_values": ["k1", "k3"]}
```

`decideApproval` checks it before anything is claimed (`narrowApproval`), and
refuses with 400, leaving the card pending, when:

- the tool declares no narrowable argument, or its staged value is not a list
  of 2 to 500 distinct non-empty strings of at most 200 characters;
- the list is empty (decline instead) or an explicit `null` (refused, never read as "approve everything"), names a value that was not staged, or
  names one twice;
- it comes with `approved: false`, or with a scope other than `once`.

Naming every staged value is a whole approval: the plain claim, the staged
call. Otherwise the card is claimed with `store.ClaimApprovalNarrowed`, which
approves the row AND writes `approvals.narrowed_json` (migration 079)
`{"arg", "staged", "approved"}` in the same UPDATE, under the same predicate as
`ClaimApproval` (pending, not expired, the caller's row). An `audit:` line
names the user, the card, the counts and the left-out staged positions;
never the values, which are model-authored and could carry anything (the
exact record is on the row).

### The run

`runStagedTool` builds the call's arguments with `approvalExecutionArgs`: the
staged arguments, with `arg` replaced by the approved values in staged order.
It re-checks the record against the staged call first (the argument holds
exactly the recorded staged values, the approved ones are a non-empty subset in
staged order) and refuses to run on any mismatch, rather than run the staged
list whole. Every other argument is decoded from the staged JSON with
`json.Number`, so large integers pass through unchanged.

The narrowed call carries MCP request metadata:

```json
"_meta": {"fleet/narrowed": {"arg": "deal_ids", "staged": ["k1", "k2", "k3"]}}
```

This is a generic MCP client feature: `mcp.WithRequestMeta` attaches `fleet/`
entries to a call's context, `Server.callTool` merges them into the request's
`_meta` beside a progress token, and the broker carries them on its own wire
(`request.Meta`); the credential-owning child keeps only `fleet/` keys.

The approved-call budget is computed for the narrowed arguments (a smaller
`deal_ids` batch gets a smaller scaled budget).

### The outcome

- The result breadcrumb in the conversation (what the model reads) is the
  tool's governed, bounded text followed by fleet's line: `[fleet] The person
  narrowed this call: of the 3 staged deal_ids in its arguments, only
  positions 2 ("k2") were left out; the other 2 were approved. Only the
  approved ones could be sent to the tool; the result says whether they ran.
  Do not run the left-out ones unless the person asks. (Written by fleet, not
  the tool.)` It records the decision only, never that anything ran (the call
  may have failed before it was sent). It names the smaller side exactly by its positions in the staged
  list (which the recorded tool call holds), and by value too when that side
  has at most 20 values, so it is exact and small (at most 250 positions). Positions
  are written as ranges ("1-3, 5"). It follows the tool's text, so a result
  card, which is read from the start of the result, is untouched. It is
  governed with the result and never truncated: the tool's text is bounded to
  what the output limit leaves after it
  (`agentcore.GovernAndBoundModelVisibleToolTextWithTrailer`), so the whole
  stays within the limit. The one exception: with an operator limit near the
  1 KiB minimum and a long, scattered narrowing (up to 250 separate
  positions, about 1.3 KiB), the line is kept whole and the result exceeds
  that limit by it, never the 128 KiB hard maximum. Exactness wins there,
  because a truncated line could let the agent run a value the person left
  out.
- A narrowed call cut off by a restart keeps the line too:
  `RecoverStrandedApprovals` reads `narrowed_json` and adds it after the
  "outcome unknown" breadcrumb, because the recorded tool call holds the whole
  staged list.
- `narrowed: {arg, staged, approved}` rides the approve POST answer (the
  early `executing` answer too), its idempotent replays, each `resolved_approvals` row and the one-card GET.
- The settled card says **Approved 2 of 3 · 1 left out** (under the result
  card's "Approved: <title>" when there is one), and the records under
  **Details** mark the left-out rows **Left out · unchecked before approval**.

### Grouped cards

`POST /conversations/{id}/approval-groups/{groupId}` takes `narrow`, a map from
an approved card's id to its ticked values:

```json
{"approve": ["ap-ix", "ap-mg"], "decline": [], "narrow": {"ap-ix": ["k2", "k3"]}}
```

Every id in `narrow` must be in `approve`, the values across all cards are at
most 5000 (the card row budget), and each narrowing is checked with the same
code as the single-card POST before any card is claimed; a bad one refuses the
whole decision. Each card is then decided with its own values. The body limit
is now 8 MiB (it was 64 KiB), which holds 5000 values of 200 characters even
fully JSON-escaped; a larger narrowing is refused and the person can decide the
cards one at a time.

On the grouped card each narrowable call shows its rows with their own
checkboxes and count. A checked call with every row unticked is declined, like
an unchecked call, and **Approve all (k)** counts the calls that will run.

## Deviations

- **Selection is per row of the describer card, not per change.** The
  designer's 2b and 2c untick single changes within a deal. Fleet narrows the
  values of one list argument, so a change can be left out only when the
  bundle makes it its own value (a plan item per change) and its own row.
- **No per-row narrowing on the generic arguments card.** Without a readable
  card there are no rows to tick; the server would accept a narrowing from an
  API client, but the web never offers one.
- **The terminal client approves whole.** `/approve <id>` sends no
  `approved_values`; narrowing from the terminal is not built.
- **"Approve k of n" counts values, "Approve all (k)" counts calls.** On a
  grouped card the per-call count line carries the per-value number.

## Deferred

- **Hot reload.** Like every `agent_policy` value the map is read at boot.
- **Adoption:** bundles adopt the key, and the describer's `key`, after this
  release, because the manifest decoder and the card parser are strict. A
  bundle whose tool binds other arguments to the list must accept
  `_meta["fleet/narrowed"]` first, or it will refuse narrowed calls (the safe
  failure).
