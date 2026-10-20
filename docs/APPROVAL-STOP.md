# Stopping a running approved call

Design note for `agent_policy.critical_tool_stop`: for chosen critical tools,
the running card of an approved call offers **Stop**. Fleet sends the MCP
cancellation for that request and keeps waiting for the server's answer; a
server that cooperates finishes the item it is on and answers normally,
saying what it did not run, and the settled card offers **Run the remaining
N**. Recorded as
[ADR-0086](adr/0086-a-person-may-stop-a-running-approved-call.md). It builds
on [APPROVAL-PROGRESS.md](APPROVAL-PROGRESS.md) (the running card and its
poll), [APPROVAL-RESULT-CARDS.md](APPROVAL-RESULT-CARDS.md) (the settled card
and its retry) and [RESUME-AFTER-APPROVAL.md](RESUME-AFTER-APPROVAL.md).

## Why

A plan's execute call can book or change dozens of deals and run for minutes
after the person approved it. Until now nothing could halt it. The designer's
state 3 has **Stop** with "Stopping finishes the current deal, then halts",
and state 6c the settled "Stopped after 12 of 24 … Run the remaining 12".

## What shipped

### The bundle opts in, per tool

```yaml
agent_policy:
  critical_tools: [execute_plan]
  critical_tool_stop: [execute_plan]
```

- Suffix matching like `critical_tools`. A member that is not critical, is
  `notify` mode, or names a handler-only card is dropped at boot with a log
  line and reported by `fleet validate-config`
  (`agentcore.ApprovalStopProblems`).
- **Absent key = today's behaviour, exactly.** No request is marked, no card
  offers Stop, no payload gains a key, and the stop route answers 400.

### The wire (a generic MCP client feature)

1. **The request says the client cooperates.** A caller attaches a stop
   signal with `mcp.WithStop(ctx, ch)`. `Server.callTool` then adds
   `_meta["fleet/cooperative_cancel"] = true` to the `tools/call`.
2. **Stop is `notifications/cancelled`.** When the signal closes, the stdio
   transport writes `{"jsonrpc":"2.0","method":"notifications/cancelled",
   "params":{"requestId":<id>,"reason":"stopped_by_user"}}` on the call's pipe,
   once, and goes on reading for the call's response. The HTTP transport
   sends the same notification as its own POST on the session while the
   call's response is still open, and only once the server has answered the
   call's headers, so the cancellation never reaches the server before the
   call it cancels.
3. **The broker carries it.** The parent marks the call `stoppable` and,
   once the call frame is sent, turns the signal into one `stop` frame with
   the call's id. The child closes the call's stop signal; unlike the
   existing `cancel` frame it does not cancel the call's context, so the call
   carries on to its answer.
4. The wait is bounded by the call's existing budget
   ([APPROVED-CALL-BUDGET.md](APPROVED-CALL-BUDGET.md)), nothing new.

The server side (the bundle) must honour the mark: when the request carries
`fleet/cooperative_cancel`, treat `notifications/cancelled` for it as "finish
the current item, then answer". Python MCP SDK servers cancel the handler at
once by default, so a bundle has to intercept the notification itself (the
Elcano bundle's deal-plan execute does this).

### The chat server

- **Armed before the claim.** An approved call of an opted-in tool gets a
  stop signal (`armApprovalStop`) before its claim makes the row executing, so
  the stop route always finds it for a running call. It is reference-counted:
  a concurrent request that loses the claim drops only its own reference, and
  the winner's goes when the outcome is recorded.
- **A restart keeps the stop.** A stopped call cut off by a restart gets the
  same fleet line after `RecoverStrandedApprovals`' "outcome unknown"
  breadcrumb.
- **`POST /conversations/{id}/approvals/{approvalId}/stop`** (web proxy
  `/api/conversations/{id}/approvals/{approvalId}/stop`):
  - 404 for an unknown card or another conversation, 400 for a tool that did
    not opt in, 409 for a card still pending (decline it instead);
  - a card already settled answers its outcome, and nothing is recorded;
  - a running card whose outcome is already being recorded (its stop window
    is finalized, or its signal is gone) answers its state and records
    nothing: accepting a stop and finalizing the outcome take the same lock,
    so the row's stop mark and the outcome's stop line always agree;
  - a running card: `store.MarkApprovalStopRequested` writes
    `approvals.stop_requested_at` (migration 080) once, only on the owner's
    row while it is executing; the call's signal closes; an `audit:` line
    names the user, the card and the tool. The answer is the card's state:
    `executing: true, stopping: true`. A second Stop answers the same and
    records nothing new.
- **A Stop before the call is sent runs nothing.** If the signal closed while
  the call's scope was still opening, `runStagedTool` does not send the call;
  the outcome is "stopped before the call was sent to its server; nothing
  ran", recorded as a failure.
- **The outcome is the call's.** Whatever the server answers is recorded by
  the same code as any approved call (governance, result card, history,
  resume, finish push). After the tool's governed, bounded text fleet adds,
  governed and bounded on its own: `[fleet] The person
  pressed Stop while this call ran. Its server was asked to stop after the
  item it was working on; the result above says what ran and what did not. Do
  not run the remaining items unless the person asks. (Written by fleet, not
  the tool.)`
- **Payloads.** `stoppable: true` rides the pending card (live event and
  GET), the early `executing` answer and the executing rows; `stopping: true`
  an executing row that was stopped; `stopped: true` a settled one. Every
  other card's payload is unchanged.
- **The running card is reachable.** An opted-in call answers `executing`
  after about 2 s (`approvalProgressEarlyReplyAfter`), as a progress call
  does, and its card polls the one-card GET until it settles.

### The card

- A running card of an opted-in tool shows **Stop** beside **Check result**,
  with "Stopping finishes the current item, then halts." as its description.
- After Stop it reads "Stopping: the current item finishes, then it halts."
  (a status region) and offers no second Stop; the poll settles it.
- A settled stopped call shows its result card (the bundle writes "Stopped
  after 12 of 24"). When every retry key names a record marked **Not run**,
  the button reads **Run the remaining N** and sends: "Run the remaining N
  items of the approved <tool> call (approval_id=…) that were not run because
  I stopped it: …. Run only these, and stage the run for my approval." As
  with **Retry failed**, nothing runs from the click.

## What a server that does not cooperate gets

- **Ignores the notification:** the call runs to its end and is recorded as
  usual, with fleet's line saying the person pressed Stop.
- **Hard-cancels and answers an error** (the Python SDK's default answers
  `Request cancelled`): recorded as a failure with that text.
- **Hard-cancels and answers nothing** (the spec's default): the call's
  budget ends the wait, recorded as any call that overran its budget.

So a bundle should opt a tool in only once its server implements the
cooperative stop.

## Deviations

- **"Halted on a shared failure" (6d) is the bundle's,** not fleet's: it is
  the server's own stop reason in its result card.
- **No Stop in the terminal client.** It shows the running state and
  `/approve <id>` fetches the outcome, as before.
- **The poll, not a push.** The card learns the outcome by polling every 2 s
  while visible, like a progress card.

## Deferred

- **Hot reload.** Like every `agent_policy` value the list is read at boot.
- **Adoption:** bundles adopt the key after this release (strict decoder),
  and only for tools whose server honours `fleet/cooperative_cancel`.
- **Stop for a call fleet's own agent loop makes** (not an approved card).
