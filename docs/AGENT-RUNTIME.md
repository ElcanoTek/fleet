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
- Sub-agents (the other half of #175 — letting the native agent spawn children
  with their own model choice) are **not** part of this and are **not yet
  shipped**.

Because the critique re-enters through the verifier's existing enforcement-round
channel (`scheduledPolicy.CanFinish`), no second governance path is created: the
review is bounded by the same per-run audit, finish enforcement, cost/token
ceilings, and round cap as everything else.

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
