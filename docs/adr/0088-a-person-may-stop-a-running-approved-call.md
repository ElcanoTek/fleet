# ADR-0088: A person may stop a running approved call; fleet cancels it cooperatively and waits for the answer

- **Status:** Accepted
- **Date:** 2026-10-20
- **Deciders:** fleet maintainers
- **Relates to:** [ADR-0042](0042-child-side-mcp-scope-authorization.md)
  (child-side MCP scope authorization), [ADR-0083](0083-a-settled-approval-card-may-start-a-turn.md)
  (resume after approval), [ADR-0084](0084-a-settled-card-reads-its-outcome-from-the-calls-own-result.md)
  (result cards), [ADR-0089](0089-an-approval-may-narrow-a-declared-list-argument.md)
  (narrowing).
- **Design note:** [`APPROVAL-STOP.md`](../APPROVAL-STOP.md)

## Context

Once a person approved a card, the call ran to its end or its budget, and
nothing in fleet could halt it. A deal plan's execute call can work through
dozens of deals for minutes. The designer's states 3 ("Stop — Stopping
finishes the current deal, then halts") and 6c ("Stopped after 12 of 24 … Run
the remaining 12") ask for a way to halt it part way without leaving a deal
half written.

The MCP spec has the tool for this, `notifications/cancelled`, but it says the
receiver SHOULD stop processing and SHOULD NOT answer, and the sender SHOULD
ignore any answer that arrives. Read literally, a cancelled call has no
outcome: fleet would record "unknown" for a plan that stopped cleanly after
item 12, and the agent could not tell which deals ran. Common server SDKs also
cancel the handler at once, which interrupts the item in flight.

## Decision

A bundle may list critical tools in `agent_policy.critical_tool_stop`. For a
running approved call of such a tool:

1. **Opt-in, announced in the request.** The approved `tools/call` carries
   `_meta["fleet/cooperative_cancel"] = true`, which tells the server that
   this client will keep reading for the call's answer after it cancels. A
   server that does not understand it is unaffected until a Stop.
2. **Stop is the spec's cancellation, plus a wait.** On Stop, fleet sends
   `notifications/cancelled {requestId, reason: "stopped_by_user"}` for that
   request (over stdio on the call's pipe; over HTTP as its own POST on the
   session; across the broker as a `stop` frame that closes the call's stop
   signal in the credential-owning child, without cancelling its context).
   It keeps waiting for the answer under the call's existing budget.
3. **Nothing else changes about the call.** It is still the claimed
   approval's one execution: the same budget, the same governance of its
   result, the same outcome write, history breadcrumb, result card and resume.
   A cooperating server finishes its current item and answers normally,
   saying in its own result what it did not run. A server that hard-cancels
   answers an error, recorded as a failure. A server that sends nothing runs
   into the budget, recorded exactly as any call that overran it.
4. **Recorded.** The stop is written to the row
   (`approvals.stop_requested_at`, once, owner only, only while the call is
   executing) and logged as an `audit:` line. A Stop that lands before the
   call is sent runs nothing and is recorded as such.
5. **The model is told.** Fleet adds one labelled line after the stopped
   call's result in the conversation, so the agent does not run the remaining
   items unless the person asks. "Run the remaining N" is a user turn that
   stages a new call for a new approval, like "Retry failed".

Without the key nothing changes: no request carries the mark, no card offers
Stop, and the stop route refuses.

## Consequences

- An approved action may now end part way, by the person's choice, with every
  item either done or not run, never half done, provided the server honours
  the cooperative contract. Fleet cannot enforce that; the bundle opts in
  only for tools whose server implements it.
- Fleet deliberately deviates from the spec's "ignore the answer after
  cancelling", and says so in the request (`fleet/cooperative_cancel`). The
  deviation is safe for a server that ignores the mark: it either answers
  (recorded) or does not (the budget ends the wait, as before).
- A running card of an opted-in tool answers `executing` after about 2 s and
  polls for its outcome, like a progress card, so Stop is reachable.

## Alternatives considered

- **Cancel the request context** (fleet's existing broker `cancel`). Rejected:
  it abandons the call and records "outcome unknown", and the server's SDK
  interrupts the item in flight.
- **A bundle "stop" tool** the card calls. Rejected: it is a second critical
  write path outside the approved call, needs its own approval and gates, and
  races the running call on the server.
- **Spec-literal cancellation** (no answer expected). Rejected: the person and
  the agent would not learn which items ran.
