# Approved-call budget: how long an approved card's MCP call may run

Design note for the budget an MCP call gets when a person approves it on a
chat approval card, and for what the approve request answers while a long
call is still running. It extends [BATCH-CALLS.md](BATCH-CALLS.md) (the
agent loop's `deal_ids` scaling, #1712) to the approval path, and
[APPROVAL-CARDS.md](APPROVAL-CARDS.md) (the card lifecycle).

## Why

An approved card runs its call outside the agent loop, in
`internal/httpapi/approvals.go` `runStagedTool`, after the click. That path
was written for SendGrid ("usually <1s") and ran the whole thing (reopening
the staged seat's scope and the call itself) under one flat
`context.WithTimeout(ctx, 60*time.Second)`. It never consulted the loop's
`toolCallTimeoutFor` and never attached `mcp.WithCallTimeout`, so the
credential-owning broker child saw `callTimeoutMs = 0`.

A deal create or update that a person approves in chat is a vendor write that
round-trips the SSP API several times. Sixty seconds cut such calls off, and a
create cancelled mid-flight has an unclear outcome. And the web approve POST
goes through the Next proxy (`chatServerFetch`, plain undici, ~300 s headers
timeout), so a call longer than about five minutes would error in the browser
even though the execution itself was already detached
(`execCtx = context.WithoutCancel`).

## What shipped

### A per-server budget, declared in the bundle

```yaml
mcp_servers:
  - name: deals_mcp
    command: …
    approved_call_timeout_seconds: 300   # one approved create/update
```

- `mcp_servers[].approved_call_timeout_seconds` is seconds, 1–1800. Absent
  (or 0) keeps the historical 60 s. The loader rejects a negative value or one
  above 1800 (most likely milliseconds typed as seconds), so
  `fleet validate-config` reports it as a blocking manifest failure.
- It follows `batch_seconds_per_deal` exactly: a `ServerDef` field under the
  strict decoder, `Bundle.AgentPolicy()` collects it keyed by manifest server
  name, and `agentcore.ConfigureAgentPolicy` installs it at boot from both
  callers (`cmd/fleet/main.go` and `internal/taskrun/taskrun.go`). A
  registered named-account variant `<server>_<account>` inherits its base
  server's value through the one server-name keying rule
  (`longestServerKey`), and its own entry wins when it has one. The engine
  matches no server by name.
- **Agent Plugin servers cannot set it.** The `com.elcanotek.fleet`
  extension's per-server allow-list is unchanged, so the key on a plugin
  server is reported as unknown and ignored.

### The budget rule (`agentcore.ApprovedCallBudget`)

`ApprovedCallBudget(serverName, rawInput) (budget, declared)`, with
`serverName` the registered name the call routes to and `rawInput` the card's
frozen arguments:

- **Not opted in:** `DefaultApprovedCallBudget` (60 s), whatever the input
  says. A `deal_ids` input does not scale it, and a `batch_seconds_per_deal`
  declaration alone does not opt a server in.
- **Opted in:** the declared budget, raised to the `deal_ids` scaling when the
  input lists records (`len(deal_ids) ×` the server's batch pace, i.e. its
  `batch_seconds_per_deal` or the 20 s default), capped at the 30-minute
  ceiling on any one MCP call. The scaling is `batchCallBudget`, the same
  helper the loop's `toolCallTimeoutFor` now uses, so there is one rule. The
  loop's 5-minute floor does not apply here: the declared budget is the
  floor.

### `runStagedTool` runs the call under it

- Reopening the staged seat's scope (spawning its server) has its own fixed
  allowance, `approvalScopeOpenTimeout` (30 s), and that context is released
  as soon as the scope is open. The close after the call keeps its own 5 s
  (`approvalScopeCloseTimeout`). Neither eats the call's budget.
- The call runs under `context.WithTimeout(mcp.WithCallTimeout(ctx, budget),
  budget)`. The per-call budget crosses the out-of-process broker as
  `callTimeoutMs`, so the credential owner applies it too, as it does for the
  loop's calls. A per-approval scope is a fresh client with no other call
  queued ahead, so the outer deadline is the budget itself, not the loop's
  budget + 5 min backstop.
- Bash, `preview_email`, `schedule_task` and `manage_tasks` are untouched.

### A long call does not hold the request open

- The claimed execution, its outcome write (`SetApprovalResult`, which also
  commits the history breadcrumb) and the session policy now run as one unit,
  `executeClaimedApproval`, on a goroutine on the detached `execCtx`. Nothing
  in it reads the request.
- For an MCP call on a server that **declared** a budget
  (`approvalMayAnswerEarly`: the card's recorded seat, or for a legacy row the
  catalog entry that would run it), the POST waits up to
  `approvalEarlyReplyAfter` (50 s, a package var tests shorten). If the call
  is still running then, the POST answers the existing executing shape,
  `{"status":"approved","executing":true,"result_text":"Approved — executing…"}`,
  and the execution finishes detached and records its outcome exactly as it
  would have. A call that finishes first answers with its outcome, as before.
  A client that disconnects while waiting gets nothing; the execution carries on.
- Every other approval (an undeclared server's email, bash, scheduling, mock
  mode) still waits for its outcome, however long that takes, exactly as
  before.
- The web and terminal clients already handled `executing`: the web card
  stays pending with `executing: true`, withholds Cancel, Edit and the expiry
  countdown, and offers **Check result**; the TUI's `/approve <id>` retrieves
  the result. No client change was needed.

### Why the existing outcome semantics still hold

- **No double execution.** The `pending → approved` claim (`ClaimApproval`)
  is still the only gate, taken before the goroutine starts. A "Check result"
  re-POST, a second tab or a double click finds the row no longer pending and
  gets `writeResolvedApprovalState`: `executing` while the sentinel stands,
  the recorded outcome after.
- **No lost result.** The outcome is written by the execution itself, not by
  the request, so an early reply or a closed tab cannot skip it. A failed
  write marks the row in `approvalPersistenceFailures`, so a later Check
  result says `execution_unknown`, never success and never "still running".
  A panic in the execution is recovered the same way.
- **Restart mid-call still reports "outcome unknown".** A process that dies
  mid-call leaves the sentinel; `RecoverStrandedApprovals` (boot only)
  rewrites it to the outcome-unknown text and appends the history breadcrumb,
  exactly as for a request-bound execution.
- **Graceful shutdown waits for it.** Executions are admitted and counted by
  `Server.approvalRuns` (`approvalRunGate`), and `performShutdown` drains them
  (`DrainApprovalRuns`) within the same grace as chat turns. Admission and
  drain share one mutex: from the moment the drain starts, a new approve POST
  is refused with 503 **before its claim**, so the card stays pending and can
  be approved again after the restart, and no execution can start behind the
  drain's back (a `sync.WaitGroup` could not promise that, because the approve
  route stays reachable while the server drains). One still running when the
  grace expires is cut off by the exit and recovered as unknown at the next
  boot. Before this change the HTTP server's handler drain covered the same
  window.
- `SweepExpiredApprovals` only claims `pending` rows, and
  `includeExecutingApprovals` lists sentinel rows, so a detached execution
  appears on reload as executing until its outcome lands.

## The workspace an approved call sees

Contract dependency: a file an MCP server writes into `${FLEET_WORKSPACE}`
(e.g. `CUTLASS_RUN_WORKDIR`) during a chat turn must be readable by the same
server when an approved card for that conversation executes. Verified on
`origin/main` 0504aa96:

- **Production (out-of-process broker): the same directory.** The interactive
  turn opens its scope with `configureTurnWorkspace`'s `fileOpRoot`, which for
  a chat turn is `tools.EnsureWorkspaceDir(convID)` =
  `tools.WorkspaceDirForConversation(convID)` (`internal/agent/manager.go`).
  The approval opens its scope with `tools.WorkspaceDirForConversation(
  approval.ConversationID)` (`approvalMCPScope`). Both strings travel as
  `ScopeSpec.Workspace` to `brokerBackend.OpenScope`, which binds the selection
  with `agentcore.BindMCPSelectionReport(..., spec.Workspace)`. That one
  function expands `${FLEET_WORKSPACE}` to the workdir
  (`ExpandWorkspaceEnv`) and chooses the cwd (`StdioCwd`) for both. The
  approval scope is a fresh server process, so only what the server wrote to
  disk carries over. Anything it holds in memory between calls does not.
- **In-process mode (no scope opener): the same directory.** The turn uses
  the shared broker and the approval falls back to the same shared broker, so
  both see `SharedMCPWorkspaceDir()` (`<root>/mcp-shared`).
- **Legacy rows with no recorded seat differ.** A card staged before seats
  were recorded (`MCPServer` empty) executes on the shared broker, whose
  servers were spawned at boot with `SharedMCPWorkspaceDir()`, while its turn
  ran on a conversation-workspace scope. Every MCP card staged by a scoped
  turn records a seat (`approvalStager.seatFor` falls back to
  `{Server: <registered name>}`), so this only affects rows from before
  #167 residual 2. Not changed here.
- **Relative workspace root (pre-existing, affects both paths equally).**
  With `FLEET_WORKSPACE_ROOT` unset (neither `deploy/fleet.service` nor the
  repo's scripts set it; the Helm chart does), `WorkspaceDirForConversation` returns the relative
  `workspace/<convID>`. That string becomes both the server's cwd and its
  `${FLEET_WORKSPACE}` value, so a server that opens `$FLEET_WORKSPACE/x`
  relative to its own cwd lands in `workspace/<convID>/workspace/<convID>/x`.
  Turn and approval agree (the file is still found), but the path is not the
  one the conversation's sandbox sees. `SharedMCPWorkspaceDir` and
  `WorkspaceRootDir` already absolutize. Absolutizing the scope workdir would
  change the turn path too, so it is a separate follow-up, not part of this
  change.
- `docs/MCP-BUNDLE-ENV.md`'s spawn-path table lists "the `fleet mcp-broker`
  process" under `SharedMCPWorkspaceDir()`. That holds for the broker's
  boot-time shared client, but a broker **scope** (every interactive turn and
  every approval in production) gets the conversation workspace. The table
  predates per-turn scopes and should get a row for them.

## Deviations

- **The scope open got its own allowance even for undeclared servers.**
  Before, open and call shared one 60 s. Now an undeclared server's call gets
  its full 60 s after a scope open of at most 30 s. That is at most 30 s more
  in the worst case, and never less for the call.
- **The early reply is opt-in.** It engages only for servers that declared a
  budget, so every other approval keeps exactly the old request shape. A
  generic early reply for every approval would also protect a slow bash or
  email, but it would change those paths. It stays deferred until there is a
  need.
- **The signature returns `(budget, declared)`.** The request path needs to
  know whether the server opted in, not just the duration.

## Deferred

- **Hot reload.** Like `batch_seconds_per_deal` and every other
  `agent_policy` value, the budget is installed at boot, so a change needs a
  Fleet restart (see BATCH-CALLS.md "Deferred").
- **HTTP MCP transports** keep their `http.Client.Timeout` of 2 minutes, which
  caps an HTTP-served server's approved call below a longer declared budget.
  The SSP bundle servers are stdio.
- **No live push of the outcome.** A card answered `executing` learns the
  result on **Check result** or on reload. The server does not push it. For a
  tool in `agent_policy.critical_tool_progress` the card polls for its progress
  and outcome, and the owner gets a browser push when it ends
  ([APPROVAL-PROGRESS.md](APPROVAL-PROGRESS.md)).
- **Bundles adopt the key after this release ships.** The manifest decoder is
  strict, so an older Fleet refuses a bundle that carries
  `approved_call_timeout_seconds`.
