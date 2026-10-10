# Grouped approvals: one card for the actions one turn stages

Design note for `agent_policy.critical_tool_group_approval`: a bundle can say
that, for chosen critical tools, the approval cards one chat turn stages are
shown as **one** card with a checkbox per call, instead of one card each. It
builds on [APPROVAL-CARDS.md](APPROVAL-CARDS.md) (the card lifecycle),
[APPROVAL-CARD-DESCRIBERS.md](APPROVAL-CARD-DESCRIBERS.md) (readable rows),
[APPROVED-CALL-BUDGET.md](APPROVED-CALL-BUDGET.md) (the detached `executing`
path) and [RESUME-AFTER-APPROVAL.md](RESUME-AFTER-APPROVAL.md) (the automatic
continue that waits for every card).

## Why

A plan that books records in several external systems stages one critical
call per system in the same turn: each call is its own tool on its own server
(and each call may already cover many records, listed on its readable card).
Every server can have a pending card at once, so the person used to get one
card per system, each with its own buttons, countdown and decision, for what
they think of as one plan. The designer's requirements R2/R3 ask for "one card
for the whole plan" with **Approve all**, **One at a time** and **Cancel**, and
a way to leave items out.

## What shipped

### The bundle opts in, per tool

```yaml
agent_policy:
  critical_tools: [execute_plan]
  critical_tool_group_approval: [execute_plan]
```

- Suffix matching exactly like `critical_tools` (the tool name equals the
  suffix or ends in `_<suffix>`, so a named-account variant is covered).
- A member that is not a critical suffix, or whose `critical_tool_modes` entry
  is `notify`, stages no card and so could never be grouped, and a
  handler-only card (`schedule_task`, `manage_tasks`, `preview_email`,
  `suggest_advanced_model`) is resolved by fleet's own handler and never
  joins a group: boot drops such a member
  with a log line, and `fleet validate-config` reports it in the
  `agent_policy` floor check (`agentcore.GroupApprovalProblems`, the same
  code).
- Installed at boot from `cmd/fleet/main.go` like every other `agent_policy`
  value. The scheduled `fleet run` path stages no cards and does not install
  it.
- **Absent key = today's behaviour, exactly.** No row gets a group id, no
  payload gains a `group_id` key, and the web renders every card on its own.

### Staging records the group

- The chat turn's stager carries the turn's id as its group id.
  `approvalStager.Stage`, after creating the row, calls `joinApprovalGroup`:
  for a tool in the list (handler-only cards never) it writes
  `approvals.group_id` (migration 075, a nullable column with no backfill)
  with `store.SetApprovalGroup`, which only touches a still-pending row the
  user owns. A failed write is logged and leaves the card ungrouped: it renders
  and resolves on its own, as before.
- `group_id` rides the live `tool.approval_required` event, each
  `pending_approvals` and `resolved_approvals` row of the conversation GET,
  and the one-card approval GET. It is only present for a grouped card, so
  every other payload is byte-identical.
- One group per turn. A resume turn (ADR-0083) is a new turn, so the cards it
  stages form a new group.

### The web renders one card

`approvalRenderItems` (`web/src/app/chat/ui/approvalGroups.ts`) decides how a
message's cards render: two or more **pending, not yet running** cards with the
same `group_id` become one `ApprovalGroupCard`, placed where the first of them
would have rendered. A lone member, a running one and a settled one render as
their own cards. The grouped card shows:

- the title **N actions to approve**;
- one row per call, checked by default: its readable card (title, records,
  footer, raw arguments under **Details**) when the bundle declares a
  describer, else `Run "<action>"` with the server, tool and arguments, as on
  the generic card; and the row's seat badge ("Runs as … on …");
- one countdown, on the earliest deadline among the calls. When it runs out,
  **Approve all** and **Cancel all** disable and a line points to **One at a
  time**, where each card shows its own state (the expired one with **Ask
  again**);
- **Approve all (k)**, k = the checked rows (disabled at 0), with a line saying
  the unchecked ones are declined; **One at a time**; **Cancel all**.

**One at a time** is client-side: the group id joins a per-page set and its
cards render individually from then on. A reload regroups any still pending.

### One decision, one request, each call on its own path

`POST /conversations/{id}/approval-groups/{groupId}` (web proxy
`/api/conversations/{id}/approval-groups/{groupId}`), body
`{"approve": [ids], "decline": [ids]}`.

1. **Validated whole before anything is claimed.** Strict JSON (unknown keys
   and trailing data refused, so no `scope` can be smuggled in), no empty or
   repeated id, at least one id, at most 100. Every named card must exist for
   the caller, be in this conversation (404 otherwise) and carry this group id
   (409 otherwise). Any failure refuses the request and claims nothing.
2. **Each card is decided by `decideApproval`**, the function behind the
   single-card POST (extracted from `handleApproval` in this change, with no
   behaviour change there), with `scope: "once"`. So every card keeps its own
   claim (`ClaimApproval`, the only gate against a double run), its own
   approved-call budget and 50 s early `executing` reply, its own outcome write
   and history breadcrumb, the expired-click rule (a card past its deadline
   settles as timed out, never runs), the shutdown drain (503 before the
   claim, the card stays pending), and its resume bookkeeping. There is no new
   execution path: an approved call still runs through
   `executeClaimedApproval → runStagedTool`.
3. **Detached.** The decisions run on `context.WithoutCancel(r.Context())`,
   up to 8 at a time. A client that goes away after the request arrived does
   not leave the decision half applied; only the wait for a running call ends
   with the connection, and the call carries on as it does for a single card.
4. **The answer** is 200 with one entry per named card:
   `{approval_id, decision, status_code, result | error}` (the approved ids
   first, then the declined ones), where `result` is
   exactly the single-card POST's body and `error` its error text. A
   top-level `"resume": true` appears when any card's answer carried it, so
   the web follows the automatic continue as after a single card. Cards of the
   group named in neither list are left as they are.
5. **Idempotent.** A replay (a lost answer, a second tab) finds the rows
   settled and answers each one's recorded state; nothing runs twice. A card
   the person decided on its own card in the meantime keeps that decision.

The web maps each entry the way the single card maps its POST answer
(`approvalStatusFromOutcome`): executing cards stay pending with
`executing: true` (and so leave the group and show **Check result**), settled
ones show their outcome card. An entry with an error leaves its card pending
and the group card names it.

#### Why a server endpoint, not a client loop

A loop of single-card POSTs from the browser was the other option. It was
rejected because it can be cut off half way: a closed tab or a dropped
connection after the first POST leaves the person's single decision half
applied (some calls approved, the unchecked ones never declined, the rest still
pending). It would also send N concurrent requests through the web proxy, each
held up to 50 s by the early-reply wait, against the browser's per-host
connection limit. One request checks the whole decision first and then applies
it on the server, detached from the connection.

### What it does not change

- **Consent stays per call.** Every call is on screen as its own row, with its
  readable card or arguments, before the person decides, and the decision
  covers only the cards named in the request. It never registers a session
  policy ("apply to all", #300): the group endpoint has no scope field and
  decides with scope `once`, so a tool in
  `critical_tool_no_session_approval` keeps its rule.
- **Resume.** The automatic continue already waits until no card of the
  conversation is pending or executing, so a group's settlements coalesce into
  one turn after the last of them. Nothing in it changed.
- **Terminal client and ACP.** They ignore `group_id` and decide each card
  with `/approve` and `/deny` as before.

## Deviations

- **The grouped card holds only the decision.** After **Approve all**, each
  call leaves the group and shows its own outcome card (running, applied,
  declined), rather than the group card turning into a combined outcome. The
  executing and resolved states, **Check result** and **Ask again** are then
  exactly the individual cards', with no second implementation.
- **One countdown, the earliest.** Cards one turn stages for the same tool
  family get the same window and are staged seconds apart, so the deadlines
  nearly coincide. When the earliest one passes, the group stops offering a
  decision and points to **One at a time** rather than deciding around it.
- **"One at a time" is not remembered across reloads.** It is a view choice;
  persisting it would need per-user UI state on the server for no change in
  what is decided.

## Deferred

- **Hot reload.** Like every `agent_policy` value the list is installed at
  boot; a card is grouped by the policy in force when it was staged.
- **Bundles adopt the key after this release ships**: the manifest decoder is
  strict, so an older fleet refuses a bundle that carries it.
- **Progress while a plan runs and a notification when it ends** (R7) is a
  separate follow-up change.
