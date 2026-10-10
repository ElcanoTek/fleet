# Approval progress: a running approved call shows how far it got, and says when it ends

Design note for `agent_policy.critical_tool_progress`: for chosen critical
tools, an approved card's MCP call asks its server for MCP progress
notifications, the running card shows them ("12 of 24 · message"), and the
conversation owner gets a browser push when the call finishes or when the
automatic continue is blocked. It builds on
[APPROVED-CALL-BUDGET.md](APPROVED-CALL-BUDGET.md) (the detached `executing`
path), [GROUPED-APPROVALS.md](GROUPED-APPROVALS.md),
[RESUME-AFTER-APPROVAL.md](RESUME-AFTER-APPROVAL.md) and
[PUSH-NOTIFICATIONS.md](PUSH-NOTIFICATIONS.md) (#292).

## Why

A bundle books a plan with one approved call per external system, and one
call can cover dozens of records, so it can run for minutes under its
approved-call budget. The card said only "Running" until the call ended, and
learned the outcome only on **Check result** or a reload. A person who walked
away had no way to know it had finished, or that the assistant was waiting for
them. The designer's requirement R7 asks for "progress while a plan runs (12 of
24 done)" and "notify the user when it finishes or needs them".

## What shipped

### The bundle opts in, per tool

```yaml
agent_policy:
  critical_tools: [execute_plan]
  critical_tool_progress: [execute_plan]
```

- Suffix matching like `critical_tools`. A member that is not critical, is
  `notify` mode, or names a handler-only card (it runs no MCP call), is dropped at boot with a log line and reported by `fleet
  validate-config` (`agentcore.ApprovalProgressProblems`).
- Installed at boot from `cmd/fleet/main.go`. **Absent key = today's
  behaviour, exactly**: no approved call carries a progress token, no row gets
  progress, no payload gains a key, no new push is sent.

### MCP progress, end to end (a generic MCP client feature)

The MCP client, a hand-rolled one in `internal/mcp`, used to read and discard
every server notification, and the broker protocol allowed exactly one
response frame per call. Now:

1. **The caller opts in with a context value.** `mcp.WithProgress(ctx, sink)`
   attaches a sink. `Server.callTool` then mints a per-call token and sends
   `_meta.progressToken` with `tools/call`, which is the spec's opt-in. Without a sink the
   request is the one it always was.
2. **The transport delivers its own notifications.** The stdio read loop, and
   the HTTP transport's SSE parser, hand each `notifications/progress` that
   carries this call's token to the sink while they wait for the response.
   Other tokens, other notifications (`notifications/message`, …) and malformed
   or non-finite values are still ignored. A message is cut to 500 runes as it
   is read.
3. **The broker forwards it.** A context value does not cross the process
   boundary. So, like `callTimeoutMs`, the parent's `mcpbroker.Client` sets
   `progress: true` on the call request, and the child re-attaches a sink
   that writes intermediate response frames (`response.progress`) with the
   call's id. The child throttles them to one per 250 ms
   (`mcp.ThrottleProgress`, which keeps the latest) and stops before writing
   the final frame. The parent's read loop hands a progress frame to the
   call's sink and keeps the pending slot for the final one.
4. In-process mode (no broker) takes the same path without step 3.

The sink runs on the transport's read path, so it must not block. The chat
server's sink keeps only the latest update and wakes a writer goroutine.

### The chat server records it

- `runStagedTool` attaches the sink (`startApprovalProgress`) to an approved
  call of an opted-in tool and stops it when the call returns, before the
  outcome is written.
- The writer stores the latest update as canonical JSON in
  `approvals.progress_json` (migration 076, nullable, no backfill), at most
  once per second (`approvalProgressWriteEvery`), through
  `store.SetApprovalProgress`. That write only touches the owner's row while it
  is still executing (approved, the executing sentinel, no outcome), so a late
  update can never land on a settled card.
- A message is made safe before it is stored and again when it is served:
  control and bidirectional-formatting characters become spaces, it is cut to
  200 characters, and a message the secret redaction would change is dropped.
  Numbers must be finite and non-negative.

- **The approve POST answers `executing` after about 2 s**
  (`approvalProgressEarlyReplyAfter`) for an opted-in MCP call that is
  still running, whether or not its server declared an approved-call budget,
  so the card starts showing progress at once. The call carries on detached
  exactly as on the 50 s path (APPROVED-CALL-BUDGET.md). Any other approval
  keeps its old reply rule.

### The card shows it

- `progress_updates: true` rides the pending card (live event and GET) of an
  opted-in tool, and each executing `resolved_approvals` row together with its
  latest `progress` (`{progress, total, message, updated_at}`). The one-card
  GET (`/conversations/{id}?approval_id=…`) answers the same. Every other card's
  payload is unchanged.
- **The web polls.** An approval runs outside any turn, and fleet has no
  conversation-level live channel (RESUME-AFTER-APPROVAL.md, "Deviations"), so
  a running card with `progress_updates` polls the one-card GET (web proxy
  `GET /api/conversations/{id}/approvals/{approvalId}`) every 2 s while the
  tab is visible (`useApprovalProgressPoll`). It shows a bar when the server
  knows the total, plus `12 of 24 · message` (`ApprovalProgressBar`), and when the
  answer says the call has finished it settles the card on its outcome by
  itself. **Check result** still works as before. The progress survives a
  reload because it is on the row.

### Browser push (internal/webpush, #292)

- **An opted-in call finished:** after its outcome is recorded
  (`executeClaimedApproval`), the owner gets `✓ Done: <label>` or
  `✗ Not applied: <label>`, at normal urgency. The label is the card's
  plain-words title when the bundle declared a describer, else the tool name.
  The call's arguments and result never go in the payload.
- **The automatic continue was blocked** (`decideApprovalResume` skipped it
  over the hourly cap, or because the conversation's queue was full): the owner
  gets `⏸ Waiting for you: the chat did not continue on its own`, at high
  urgency. The resume claim now returns the conversation owner for this. A
  deleted conversation sends nothing.
- **A per-conversation deep link.** Chat pushes now open `/chat?c=<id>`, which
  the chat page already consumes on load (the "Discuss this run" bridge), under
  `FLEET_PUBLIC_URL` or relative to the app. That includes the existing
  "approval needed" push, which used to open the app root. The service worker
  focuses an open fleet window and navigates it to the conversation, or opens
  a new window when it cannot (a window it does not control).
- Gated like the existing approval push: Web Push configured (the VAPID keys)
  and `FLEET_PUSH_ON_APPROVAL_REQUEST` not off. Sent fire-and-forget on the
  server's background tracker, so shutdown waits for it.

## Deviations

- **Polling, not SSE.** The request said "store/SSE". There is no live channel
  for a conversation outside a turn. Adding one is the deferred item in
  RESUME-AFTER-APPROVAL.md, and it is bigger than this change. The row is the
  store, and the card reads it every 2 s while visible and running. That is one
  small authenticated GET per running card.
- **The finish push is sent for every opted-in call that ends,** including a
  quick one the person watched. Telling "the person is looking" from "the
  person walked away" would need presence tracking, which fleet does not have.
- **Progress frames are throttled twice** (250 ms in the broker child, 1 s for
  the store), and the last update before the call ends may be dropped. The
  outcome replaces it.
- **The agent loop's own MCP calls do not ask for progress.** The mechanism is
  generic, but only approved calls of opted-in tools use it.

## Deferred

- A live conversation-level event channel, which would replace the poll.
- Progress for the agent loop's own long tool calls (shown on the tool chip).
- Per-user notification preferences, as already deferred in
  PUSH-NOTIFICATIONS.md.
- **Hot reload:** like every `agent_policy` value, the list is read at boot.
- **Adoption:** bundles adopt the key after this release, because the manifest
  decoder is strict, and a server reports progress only when it sends
  `notifications/progress` for the token it receives.
