# The fleet agent runtime

fleet runs **one** native agent loop, in the fleet process. Every tool call it
makes — `bash`, `run_python`, file I/O, MCP — executes inside a rootless-Podman
sandbox, and the credentials those calls need are brokered **host-side** and
never enter the sandbox. There is no flavor picker and no external-agent
delegation: the loop, the sandbox, and the credential broker are the whole story.

This guide documents the runtime mechanics an operator needs: the per-turn
sandbox seal, the cost/token ceilings, context-window compaction, the per-task
MCP credential allowlist, the scheduled end-of-run verifier, the optional
"phone a friend" super-LLM review, and git-worktree isolation for scheduled tasks.

---

## The per-turn execution sandbox

Every `bash` / `run_python` call runs inside an ephemeral rootless-Podman
container over a persistent per-conversation workspace. The agent loop itself
runs in the fleet process, but it holds no privileged executor — each tool call
is handed to the sandbox under full host policy (cost ceilings, repeat
detection, critical-tool approval staging, the email send gate, …), and fleet
records the real, executed tool calls as the audit trail, not a self-report.

**MCP credentials never enter the sandbox.** MCP tools are advertised to the
model, but every `mcp_*` call is executed host-side against the per-task
credentialed client by the **out-of-process MCP broker** (issue #167). The
sandbox holds no MCP credential; the broker injects the right credential at the
moment it runs the call and the value never travels into the container or the
model's context.

### Lockdown / network-sealed sandboxes

Lockdown bounds *tool execution*, not the model call. A lockdown turn hands the
agent a **no-network** (`--network=none`) per-turn sandbox, so the tool calls
cannot reach the network while the model loop continues normally. Scheduled
runs default to this sealed posture — see below.

---

## Cost and token ceilings

Each turn runs against configurable per-task cost and token **ceilings**, an
iteration cap, and a per-turn timeout. They are enforced, not advisory: a model
that won't stop calling tools is stopped by the ceiling. Usage and cost are
accumulated as the agent works and checked against the spec ceilings in-loop, so
a runaway loop costs a capped turn, not an open-ended invoice. The observer
events persist as a per-turn audit trail answering "what did this agent do, and
what did it cost?".

---

## Context-window pressure (proactive compaction)

Before each round's model call the run loop compares the prompt size against the
active model's context window (resolved by `contextWindowForModel` — observed
provider ground truth, the live OpenRouter cache, then a static table) and acts
**before** the provider rejects an oversized request, rather than only recovering
after a `context_length_exceeded` error:

| Env var | Default | Behavior at/above the fraction |
|---|---|---|
| `FLEET_CONTEXT_PRESSURE_WARN_THRESHOLD` | `0.75` | Emit a `fleet.context_pressure` SSE event (the chat UI shows a non-blocking "conversation is N% full" banner). |
| `FLEET_CONTEXT_COMPACTION_THRESHOLD` | `0.90` | Proactively summarize the **oldest half** of the history (pinned head + recent half kept verbatim) and emit `fleet.context_compacted`. |

Both honor the usual `CHAT_`/`CUTLASS_` prefix aliases, and a value outside
`(0,1]` falls back to its default. The size signal is the **per-call** input
size (`LastStepPromptTokens`), never the cumulative token total, so a long run
does not ratchet the trigger into a compaction spiral; on a turn's first round —
before any per-call size is known — it falls back to a char-heuristic estimate of
the carried-over history so a single-round turn that *starts* near the limit is
still covered.

**Scheduled safeguard.** Unattended runs must not silently rewrite their own
transcript: in `ModeScheduled` the warn event still fires (and a breadcrumb is
written to the session log), but proactive compaction is **off** unless the
operator sets `FLEET_SCHEDULED_AUTO_COMPACT=1`. The summary uses the driver's
`compactionSummarizer` (an LLM summary) when wired, else a deterministic
placeholder — the same hook the reactive `context_length_exceeded` recovery path
already uses, so a proactive compaction does not count toward the consecutive-
compaction cap that guards against compaction loops.

---

## Per-task credential allowlist (least-privilege MCP)

A scheduled task's MCP selection (`mcp_selection`) controls *which servers* it
sees; the **credential allowlist** (#184) additionally scopes *which
`(server, account)` credential pairs* it may call — Gate-3, after the server
opt-in (Gate-1) and per-server tool allowlist (Gate-2):

- `credential_allowlist: null` (the default) → **inherit global**: any server in
  `mcp_selection` is permitted (unchanged behaviour).
- `credential_allowlist: []` → **deny all** MCP calls.
- `credential_allowlist: [{"server":"github","account":"client-a"}, {"server":"sendgrid"}]`
  → only those pairs. A `{"server":"sendgrid"}` entry (no account) matches **only
  the default seat**; a named account must be enumerated explicitly.

A call to a non-permitted pair is **denied before it executes**: the tool is
advertised but every invocation returns a governance message to the model (a
tool result, not a transport error) and records an audit entry
(`credential_allowlist_denied`). The allowlist stores pair **names** only —
credential values never enter the database (they live in the host env file; see
[`internal/creds`](../internal/creds)).

Set or clear it with the admin CLI (the task must be pending/scheduled):

```sh
fleet-admin sched task set-credentials <task_id> --allow github:client-a --allow sendgrid
fleet-admin sched task set-credentials <task_id> --clear   # revert to global inherit
```

---

## Per-persona tool allowlist (least-privilege by role)

Different personas have different roles and risk surfaces. A `code-reviewer`
persona that can send email, or an `executive-assistant` that can run arbitrary
shell commands, violates least privilege. The **per-persona tool allowlist**
(#294) lets the bundle manifest declare, per persona, which tools that persona
may see — Gate-4, layered on top of the server opt-in (Gate-1), the per-server
tool allowlist (Gate-2), and the per-task credential allowlist (Gate-3).

Declare it in the manifest's `personas:` block (the persona `name` matches the
basename of its `personas/<name>.yaml` file):

```yaml
personas:
  - name: code-reviewer
    tool_permissions:
      allow:
        - bash
        - run_python
        - mcp:filesystem/*
      deny:
        - mcp:email/*
        - send_email
  - name: executive-assistant
    tool_permissions:
      deny:
        - bash
        - run_python
```

Pattern syntax (matched against the fantasy tool name — a native name like
`bash` or the `mcp_<server>_<tool>` form discovered MCP tools register under):

| Pattern | Matches |
|---|---|
| `bash` | the native tool named `bash`, exactly |
| `mcp:server/tool` | one MCP tool (→ `mcp_<server>_<tool>`) |
| `mcp:server/*` | every tool from one MCP server |
| `prefix/*` | any tool whose fantasy name has that prefix |
| `*` | every tool |

Resolution rules:

- **No `tool_permissions` block (or both lists empty)** → no narrowing; the
  persona sees every tool the earlier gates already permit (backward compatible —
  the generic bundle ships no `personas:` block, so behaviour is unchanged).
- **`allow` non-empty** → default-deny: only matching tools are offered.
- **only `deny`** → default-allow: every tool except matching ones is offered.
- **Deny takes precedence** when a tool matches both lists.

**This gate can only NARROW, never widen.** The filter runs over the tool list
that already survived Gates 1–3, so a tool a persona's `allow` names but that the
server or credential gates already dropped never reappears — the allowlist
subtracts from, but can never add to, what the run was already permitted to
offer. Enforcement is at **tool registration, before the first LLM call**: a
suppressed tool never enters the model's tool list (a tool the model cannot see
cannot be hallucinated into a call). Each suppressed tool emits a
`persona_tool_blocked{persona, tool, reason}` observer event for the audit
trail. **Credentials are unaffected** — they stay host-side, brokered
out-of-process; this gate only decides which tool *schemas* are advertised.

---

## Scheduled execution: sealed by default

A scheduled task's `bash` / `run_python` execution sandbox runs with **no
outbound network egress** (`--network=none`) by default — the same seal the
interactive lockdown path applies. Unattended runs have no human on the loop, so
the safe posture is the default: a scheduled task cannot fetch arbitrary URLs,
`pip install`, reach host-local services, or exfiltrate unless you opt it in.

To let a specific task's sandbox reach the network, set `allow_network: true`
on the task (the **Allow network egress** toggle in the task-create form, or the
`allow_network` field on `POST /tasks`). The default is `false` (sealed); the
opt-in is per-task, so one task needing egress does not open up the rest. This
governs only the execution sandbox's `--network`; it never affects credential
brokering, which always stays host-side.

---

## Dead-letter queue (#253)

A scheduled task retries on transient failures up to its `max_retries` budget
(the backoff curve + which failure classes retry come from the per-task
`retry_policy`, #201). Once that contract reaches a **terminal** failure — either
a transient failure with the retry budget exhausted, or a non-retryable
(deterministic) failure — the runner routes the task to a distinct
`dead_lettered` terminal status instead of bare `error`, so the exhausted task is
**reviewable and replayable** rather than silently failing. The row records when
it was quarantined (`dead_lettered_at`), the final attempt's failure message
(`dead_letter_reason`), and the total attempts made (`dead_letter_attempts`). The
runner is the only writer of this status; a self-reporting worker cannot set it.

`error` is preserved for the **non-final** failure cases it always covered —
per-attempt failures that will retry, an interrupted run (shutdown grace
expired), and a panic during execution.

Review and replay are operator actions on the box, via the admin CLI:

```sh
fleet-admin sched dlq list [--tag <tag>] [--limit N] [--offset N] [--json]
fleet-admin sched dlq replay <task_id>   # reset to pending; the scheduler re-runs it
```

`replay` resets the same task to a fresh pending slate (`attempt_count = 0`, the
dead-letter columns cleared) and the normal claim path re-runs it. Entry into the
DLQ also increments the `fleet_dead_letter_queued_total{reason}` counter (reason
is the bounded class `retry_exhausted` or `non_retryable` — deliberately not a
per-task label, to avoid unbounded metric cardinality). Dead-lettered tasks are
**not** subject to the automatic retention sweep — quarantine is for review, so a
DLQ task persists until it is replayed (or explicitly removed).

---

## The scheduled end-of-run verifier

Scheduled runs layer an extra host-side LLM re-check on top of the shared
audit/finish enforcement. When the scheduled policy clears a run, the
`runEndOfRunVerifier` runs on fleet's fallback model (host-side creds — the
verifier's model call is just another host LLM call) and returns any missing
required actions, which the loop turns into a final enforcement round before it
is allowed to finish. A verifier error fails **open** (allow finish). So core
governance — per-tool policy, audit, finish enforcement, MCP credential
brokering, note staging, usage/cost, **and the end-of-run verifier** — applies to
every scheduled run; a run never silently finishes unverified.

### Iterative verification loops

A scheduled task with a `loop_config` (#179) runs as a bounded
**worker → verify → retry** loop instead of a single pass: each iteration runs
the worker agent to completion, evaluates an exit condition, and — if it fails
and budget remains — re-runs the worker with the prior output fed forward, up to
`max_iterations` (default 5). A task with no `loop_config` is an ordinary
one-shot run (unchanged).

The exit condition (each iteration is judged by exactly one):

- `shell:<cmd>` — run `<cmd>` in the worker's sandbox; exit 0 = pass.
- `regex:<pattern>` — match `<pattern>` against the worker's last assistant message.
- `llm` — ask `verifier_model` (defaults to the task's fallback model) the
  `verifier_prompt`; a reply beginning with `YES` = pass.

Two ceilings stop a runaway loop, **checked before each iteration** so
already-accrued cost counts: `max_cost_usd` (accumulated across iterations) and
`time_budget_seconds` (absolute wall-clock). Each iteration is the **same
governed worker pass** an ordinary scheduled task uses (the loop adds only the
verify/retry control around `agentcore.Run`), so the sandbox, policy, audit, and
cost gates apply per cycle — "governance is one core" holds. Per-iteration
telemetry (status, exit result, cost, tokens) is recorded to `task_iterations`
and embedded in the `GET /tasks/{id}` response for a looped task.

---

## "Phone a friend": one-time super-LLM review (#175)

An **optional, off-by-default** quality gate that layers onto the SAME finish
seam as the verifier. When `FLEET_PHONE_A_FRIEND_ENABLED` is set, a scheduled run
that has already cleared audit/finish enforcement **and** the end-of-run verifier
is reviewed once more by a configurable — typically stronger — **reviewer model**
(inspired by Brad's [lifeline](https://github.com/bradflaugher/lifeline) MCP: a
one-time second opinion from a more capable model). `runPhoneAFriendReview`
sends the original task, the agent's final answer/work, and the executed-tool
summary to the reviewer and asks for a JSON verdict
(`{"needs_revision", "issues", "reasoning"}`); when the reviewer flags material
problems, the loop turns the issue list into **one more enforcement round** so
the agent revises before finishing.

What it is and is **not**, stated plainly (honesty in docs):

- It is a **host-side LLM call**, exactly like the verifier — the reviewer's
  credentials are just another host model handle and **never** enter the sandbox,
  the agent's model context, or logs (raw output is clamped to a short preview
  before any log line). It is **not** a built-in agent tool and **not** an MCP
  server, so the agent cannot invoke the reviewer at will, unbudgeted, or surface
  it in the sandboxed tool roster — keeping governance one core.
- It runs **at most once per run** and **fails open**: a reviewer error, an empty
  reply, or an unparseable verdict logs a skip and allows the run to finish, so a
  flaky reviewer never blocks otherwise-complete work.
- It is **scheduled-only** and gated: with the flag off (the default), the review
  never runs and behaviour is identical to before. The reviewer model slug comes
  from `FLEET_PHONE_A_FRIEND_MODEL` and **falls back to the run's fallback model**
  when unset; an unresolvable slug also falls back rather than failing the run.
- Sub-agents (the other half of #175) are a separate capability — see below.

Because the critique re-enters through the verifier's existing enforcement-round
channel (`scheduledPolicy.CanFinish`), no second governance path is created: the
review is bounded by the same per-run audit, finish enforcement, cost/token
ceilings, and round cap as everything else.

---

## Governed sub-agents (#175)

An **optional, off-by-default** capability (`FLEET_SUBAGENTS_ENABLED`) that adds a
`spawn_subagent` native tool so a scheduled run can delegate a scoped subtask to a
**child** run. The child is **not** a new or weaker loop — it is another
`agentcore.Run`, governed exactly like the parent (see
[ADR-0007](adr/0007-governed-sub-agents.md)). The tool body
(`internal/agent/subagent.go`) only adapts I/O around a fresh
`agent.Agent.Execute`.

Each spawn obeys four non-negotiable properties:

- **Governance is one core.** The child runs through `(*Agent).Execute → agentcore.Run`
  — the same governed entrypoint, pinned by `TestEntrypointConformance`.
- **Monotonic privilege.** The child inherits the parent's sandbox (so the same
  network-seal posture — it has no namespace of its own to widen), the parent's
  brokered MCP client, and the parent's MCP/credential allowlists, and may only
  **subtract** (an `allow_servers` request is intersected with what the parent has
  loaded; the credential allowlist is the parent's, copied). A per-child model is
  resolved **host-side** like the phone-a-friend reviewer, so credentials never
  enter the sandbox or model context.
- **Hard budget split.** The child's cost/token ceiling is **sliced from the
  parent's remaining budget**, and the child's actual spend is **charged back**
  into the parent. The parent's configured `MaxCostUSD`/`MaxTotalTokens` is the
  hard wall the collective spend of all descendants — across fan-out *and* depth —
  can never breach.
- **Recursion / fan-out caps.** `FLEET_SUBAGENTS_MAX_DEPTH` (default 2) bounds
  recursion and `FLEET_SUBAGENTS_MAX_CHILDREN` (default 4) bounds per-parent
  fan-out; a spawn exceeding either is **refused with a tool error**, never a
  panic or silent allow.

Stated plainly (honesty in docs): with the flag off (the default), the tool is not
even registered and behaviour is identical to before. The budget split combines an
**atomic up-front reservation** of each child's granted ceiling (held against the
parent's remaining budget under the parent mutex for as long as the child runs)
with **charge-back** of the child's actual spend on return. Because the
reservation is atomic, even N **concurrent** spawns can never collectively be
granted more than the parent has left — the wall does **not** rely on
`spawn_subagent` being a sequential tool (a concurrency regression test pins this
under `-race`; see ADR-0007). `FLEET_SUBAGENTS_MODEL` names a default child model
slug; empty means the child inherits the parent's model.

---

## Git worktree isolation (scheduled tasks)

A scheduled task with a `worktree_config` (#180) runs each occurrence in its own
git **worktree + branch**, so two tasks targeting the same repository can't
corrupt each other's working tree (dirty files, colliding checkouts). A task with
no `worktree_config` shares the workspace root, unchanged.

```json
{
  "worktree_config": {
    "enabled": true,
    "base_branch": "main",          // empty = repo HEAD
    "branch_prefix": "fleet/task-", // empty = "fleet/task-"
    "auto_cleanup": true,           // remove worktree + branch after the run
    "cleanup_delay_seconds": 0      // delay before removal (0 = immediate)
  }
}
```

The task's workspace must be the **root of a git repository**; a non-repo (or a
non-root subdirectory) is rejected at task creation. Each run gets a deterministic,
unique branch `"<branch_prefix><task_id>-<run_id>"` and a worktree checkout, so
concurrent runs never collide and no locking is needed. For a looped task (#179)
the worktree is created **once per task** and reused across iterations, so
filesystem state accumulates the way it does for a shared-workspace loop.

**Where the worktree lives — and why it is NOT `/tmp`.** The worktree is created
as a subdirectory of the workspace root (`<workspace>/.fleet-worktrees/<task>-<run>`),
*not* at a standalone `/tmp` path. A git worktree's `.git` is a file pointing back
to `"<mainrepo>/.git/worktrees/<name>"`; git only resolves it when **both** the
worktree and the main repo are reachable at their host absolute paths inside the
sandbox. The sandbox bind-mounts the workspace root at the same absolute path, so
a subdir of it satisfies that linkage — a lone `/tmp` worktree would break git
inside the container because the main repo would be unmounted. The subdir is kept
out of the main tree's `git status` via `.git/info/exclude` (a local, never-committed
exclude). The run is scoped into the worktree by two complementary host-side
seams: the per-run sandbox's default working directory and a per-run forced
working directory threaded into the in-process tool layer. Together they scope
**bash, run_python, and the relative-path file tools** into the worktree — git
operations (driven through bash, the point of the feature) are isolated.

**Cleanup.** With `auto_cleanup: true` the worktree and its branch are removed
after the run (optionally after `cleanup_delay_seconds`); with `false` the branch
is left in place for inspection or a manual push. Orphans from a crashed run (the
process died between worktree creation and cleanup) are reclaimed by an operator
with `fleet-admin worktree prune --older-than <dur>` (and `fleet-admin worktree
list` shows all registered worktrees).
