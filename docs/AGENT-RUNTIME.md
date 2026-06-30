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

### Hypervisor-isolated runtimes (Kata / libkrun, #217)

By default the per-turn container runs under Podman's shared-kernel OCI runtime
(`crun`/`runc`). A deployment handling untrusted prompts or sensitive data can
raise the isolation posture to a **dedicated KVM VM per tool call** by setting
the bundle manifest's `sandbox.runtime` (or `FLEET_SANDBOX_RUNTIME`) to `kata`
or `libkrun` — an escape then requires a hypervisor CVE, not just a
container-escape. fleet emits the value as `podman run --runtime=<value>`,
**fail-closed preflights** `/dev/kvm` + the runtime binary at boot (a missing
KVM aborts startup rather than degrading to a shared-kernel container), and adds
the Kata guest-memory overhead so the `--memory` cap still reflects usable guest
RAM. Everything else — credentials staying host-side, seccomp, dropped caps,
network sealing, per-task limits — is unchanged. See
[`SANDBOX-RUNTIMES.md`](SANDBOX-RUNTIMES.md) and
[ADR-0010](adr/0010-microvm-sandbox-runtimes.md).

### `run_python` kernel lifetime (per-turn vs persistent, #213)

`run_python` executes in a long-lived IPython kernel inside the sandbox, so
multiple calls **within one turn** already share state. What `FLEET_PYTHON_REPL_MODE`
controls is whether that kernel survives **between turns**:

- **`per-turn` (default):** the kernel dies with the per-turn sandbox at turn
  end. Variables/imports do **not** carry into the next turn — write to the
  workspace to persist anything. Unchanged legacy behaviour.
- **`persistent`:** one sandbox + kernel is kept alive **per conversation**
  (keyed by conversation ID) and reused across that conversation's turns, so a
  `DataFrame` built in turn 1 is still in scope in turn 3 with no re-read. It is
  **never shared across conversations** — that is the real isolation invariant
  (see [ADR-0008](adr/0008-persistent-python-repl-per-conversation.md)). Lockdown
  turns and scheduled runs always stay per-turn. Pass `reset_kernel=true` to wipe
  a persistent kernel back to a clean slate mid-conversation.

A persistent sandbox is reclaimed on conversation delete, on process shutdown,
and by an idle reaper (`FLEET_PYTHON_REPL_IDLE_TTL`, default **1800s**); the
number of live persistent sandboxes is capped at `FLEET_PYTHON_REPL_MAX`
(default **32**, LRU-evicting the least-recently-used idle one). It carries the
same cgroup memory/CPU/PID/disk caps as a per-turn sandbox.

Independent of mode, `FLEET_PYTHON_CELL_TIMEOUT` (default **0** = disabled) is a
host-operator ceiling on a single cell; the effective per-cell timeout is
`min(the call's timeout_seconds, this)`.

**Inline figures.** When the kernel emits an `image/png` (e.g. `plt.show()` /
`display(fig)`), the bridge writes it to a `figures/` subdir of the conversation
workspace under a server-generated filename and returns only the small relative
path in the result's `image_files`. The chat UI renders it inline via the same
authenticated workspace-file proxy that serves `![](chart.png)` — so the agent
needs no `plt.savefig()`, and the (large) base64 bytes never enter the model's
tool result. Bounded to 20 figures / 10 MiB each per cell.

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

## Approval timeouts: default-deny on no answer (#225)

High-risk tool calls (outbound email, risky shell commands, the advanced-model
nudge) are **staged** for human approval rather than executed directly. A staged
approval carries an `expires_at` deadline; if no one answers in time it is
**auto-denied** — a default-DENY-on-timeout contract so a card a user walks away
from never silently lingers as an executable action. A background sweep
(every 30s) flips expired pending approvals to *rejected* and writes the outcome
into the conversation so the next turn knows the action was not taken. A human
who clicks Send in the brief grace window before the sweep still wins the race
(the atomic claim decides), so a late decision is honored rather than lost.

The wait window resolves highest-priority-first:

1. **Per-tool** — `agent_policy.critical_tool_timeouts` in the client bundle
   manifest (keyed by the same bare tool-name suffix as `critical_tools`).
2. **Per-conversation** — `POST /conversations/{id}/approval-timeout` with
   `{"approval_timeout_seconds": N}` (or `null` to clear).
3. **Global** — `FLEET_APPROVAL_TIMEOUT_SECONDS` (default **300**). A non-positive
   value is treated as "use the 300s default", never as "deny instantly".

`FLEET_AUTO_APPROVE_IN_TEST` (default **false**) is a CI/test escape hatch that
auto-approves every staged critical tool instead of waiting for a human. It
**bypasses the human-in-the-loop gate** and is intended only for pipelines with
no human present and a mocked backend — never enable it in production. fleet logs
a loud warning at startup when it is on.

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
fleet sched task set-credentials <task_id> --allow github:client-a --allow sendgrid
fleet sched task set-credentials <task_id> --clear   # revert to global inherit
```

---

## Per-user remote MCP servers (OAuth login, #443)

The bundle's MCP servers are operator-provisioned. fleet also lets each **user**
add a **remote (hosted) MCP server** from the GUI and log in to it via the MCP
OAuth handshake (spec revision 2025-06-18: OAuth 2.1 + PKCE S256, RFC 9728/8414
discovery, RFC 7591 dynamic client registration, RFC 8707 resource indicators).
The connected server's tools then participate in that user's chat turns **and**
their scheduled tasks. In chat they appear in the Tools picker as toggleable,
default-on entries (gated per conversation exactly like a bundle Optional
server, so they count against the tool ceiling only when selected); scheduled
runs use all of the owner's connected servers. Local stdio servers are
unchanged. See [ADR-0009](adr/0009-per-user-remote-mcp-oauth.md) for the full
rationale.

The feature is **off until configured** and fails closed:

- `FLEET_MCP_OAUTH_ENCRYPTION_KEY` — base64 of 32 random bytes
  (`openssl rand -base64 32`). Encrypts the per-user tokens / client secret at
  rest (AES-256-GCM, AAD-bound to `(email, canonical server URL)`). Unset → the
  feature is disabled and the endpoints report it.
- `FLEET_PUBLIC_BASE_URL` — the externally-reachable web origin
  (e.g. `https://fleet.example.com`). The OAuth redirect URI is derived from it
  (`<base>/api/oauth/mcp/callback`) and must be byte-stable; it is **never**
  reconstructed from request headers. Required.
- `FLEET_REMOTE_MCP_ALLOW_INSECURE_HTTP` — dev only; permits `http://` servers.
  Default false (https required).

How the invariants hold:

- **Credentials stay host-side (ADR-0003).** Tokens live only in the fleet
  process + chat Postgres (encrypted) and reach a server only as the
  `Authorization` header the host-side MCP client writes — never the sandbox,
  model context, or logs.
- **No forked governance (ADR-0001).** A user's servers are wired as a *per-run
  overlay* `mcp.Client` (built with a freshly-refreshed bearer) composed with the
  shared/bundle client via a `compositeBroker`. The shared long-lived client is
  never mutated with per-user secrets, so concurrent users can't cross-pollute.
  Chat and scheduled use the same overlay + refresh path.
- **SSRF guard.** User-supplied URLs are dialed through a client that rejects
  private/loopback/link-local/metadata IPs at connect time (DNS-rebinding safe)
  and refuses redirects.
- **Rotation-safe refresh.** Tokens are refreshed under a `SELECT … FOR UPDATE`
  row lock with a post-lock expiry re-check, persisting any rotated (single-use)
  refresh token in the same transaction. A dead refresh token marks the
  connection `needs_reauth` and the server is skipped — the run still completes.

Scheduled tasks resolve the **task owner's email** (the orchestrator username)
to look up that user's connected servers, so a headless run reaches them with no
user present — as long as a valid refresh token exists. A headless run can't
re-prompt the user to log in, so a needs-reauth server is skipped AND surfaced
to the owner: a notice naming the unavailable connectors is prepended to the run
(visible in the task transcript) so the agent doesn't silently rely on missing
tools and the owner knows to reconnect.

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

## Captain's Log: persistent task memory (#198, #285)

By default every run of a scheduled task starts **cold** — it has no knowledge of
what prior runs observed or decided. A task can opt into persistent, task-scoped
memory by setting `instruction_self_improve: true` (the **Captain's Log** toggle
in the task-create form). When set, that task's scheduled runs get two extra
native tools:

- `remember(key, value)` — upsert a fact for this task. Committed immediately;
  scheduled runs are unattended, so there is no human-approval step.
- `recall(key?)` — read one fact, or all of them as a JSON object.

At the start of every run, all of the task's stored facts are injected into the
system prompt under a **Your Persistent Memory** section, so the agent sees prior
state without having to call `recall`. This lets a recurring task track state
across time — "alert only if the price changed since last week", "skip anomalies
already triaged", "accumulate a running digest".

The store is bounded so a long-lived task cannot grow unbounded:
`FLEET_TASK_MEMORY_MAX_KEYS` (default 100, oldest key evicted LRU-style on
overflow) and `FLEET_TASK_MEMORY_MAX_VALUE_BYTES` (default 4096, a hard reject).
Memories live in the **scheduler database** (`task_memories`, keyed by
`(task_id, key)`), not the client-config bundle — this is runtime state, so it
never touches the operator-owned, git-versioned bundle, and the reproducibility
guarantee ("the setup that worked is the setup that runs again") is preserved.
Inspect or clear a task's memory with `fleet task memories list|clear|delete`.

Off by default (the column is `BOOLEAN NOT NULL DEFAULT FALSE`), so a task that
does not opt in behaves exactly as before — no extra tools, no injection.

Prompt/knowledge self-improvement is a separate, already-shipped path: the agent
proposes edits to the admin-curated knowledge base via `propose_note`, an admin
publishes or rejects them, and published notes are injected into every run's
prompt. Agent-authored client-bundle **skills** are intentionally *not* part of
this — skills stay operator-authored so the bundle remains a reproducible
artifact; nothing fleet does ever writes the bundle or commits to git.

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
fleet sched dlq list [--tag <tag>] [--limit N] [--offset N] [--json]
fleet sched dlq replay <task_id>   # reset to pending; the scheduler re-runs it
```

`replay` resets the same task to a fresh pending slate (`attempt_count = 0`, the
dead-letter columns cleared) and the normal claim path re-runs it. Entry into the
DLQ also increments the `fleet_dead_letter_queued_total{reason}` counter (reason
is the bounded class `retry_exhausted` or `non_retryable` — deliberately not a
per-task label, to avoid unbounded metric cardinality). Dead-lettered tasks are
**not** subject to the automatic retention sweep — quarantine is for review, so a
DLQ task persists until it is replayed (or explicitly removed).

---

## Published output artifacts (#204)

A scheduled run's agent can mark files it produced in the workspace as **named
output artifacts** via the `publish_artifact` tool — a *curated* manifest of
deliverables (the report, the processed dataset, the rendered document),
distinct from the raw per-run workspace the file-browser endpoints already
expose. The agent writes a file, then publishes its workspace-relative path with
an optional description; the tool validates the path stays inside the workspace
(no traversal / symlink escape) and names an existing regular file, then records
`{name, path, description, size}`. It never reads, copies, or moves the bytes —
the file stays in the workspace.

The tool is **scheduled-only** (assembled beside `create_task` / the metadata
tools) and **ungated**: it can only record files in the run's own workspace,
which the operator can already browse, so it grants no new access. A per-run cap
bounds the manifest; re-publishing a path updates it in place.

The manifest is persisted on the run's **success path**, under the held lease,
just before the terminal transition (riding a running-status update like the
structured-output capture, #244) into a nullable `artifacts` JSONB column. The
column is deliberately excluded from the task upsert, so a later status update
cannot clobber it. `GET /tasks/{id}/artifacts` returns the manifest (404 when the
run published none; 409 while non-terminal); each entry's path is downloadable
via the existing workspace file endpoint. Because the manifest indexes the
creator-private workspace (#287), the endpoint is gated to the task's **creator
or an admin** — the same ownership check as the workspace file endpoints, not the
looser task-visibility used by `/output`. A re-run (lease recovery) clears the
prior attempt's manifest, so a task only ever serves the artifacts of its
latest, successful attempt.

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

## Governed sub-agents / agent delegation (#175, completed by #264)

An **optional, off-by-default** capability that adds a `spawn_subagent` native tool
so a scheduled run can delegate a scoped subtask to a **child** run — the agent
delegation issue #264 asks for, realized as this one tool rather than a second
`delegate_task` entrypoint (a second tool would be the forked, weaker path
[ADR-0001](adr/0001-one-governed-run-loop.md)/[ADR-0007](adr/0007-governed-sub-agents.md)
forbid). The child is **not** a new or weaker loop — it is another `agentcore.Run`,
governed exactly like the parent. The tool body (`internal/agent/subagent.go`) only
adapts I/O around a fresh `agent.Agent.Execute`.

**Two ways to turn it on (either suffices):**

- **Per task** — set `allow_delegation: true` on the scheduled task. This is the
  granular opt-in (#264): the tool is registered for that task even when the
  fleet-wide flag is off.
- **Fleet-wide** — `FLEET_SUBAGENTS_ENABLED` is the operator override that enables
  it for every scheduled task (the #175 behaviour, retained).

They compose as **OR** (`FLEET_SUBAGENTS_ENABLED || task.allow_delegation`). With
both off (the default) the tool is **not even registered** and behaviour is
identical to before. Delegation is honoured **only in scheduled mode** — it is
never registered in interactive chat, regardless of config (parallel unattended
sub-agents are too expensive/unpredictable for a live session).

Each spawn obeys these non-negotiable properties:

- **Governance is one core.** The child runs through `(*Agent).Execute → agentcore.Run`
  — the same governed entrypoint, pinned by `TestEntrypointConformance`.
- **Monotonic privilege.** The child inherits the parent's sandbox (so the same
  network-seal posture — it has no namespace of its own to widen), the parent's
  brokered MCP client, and the parent's MCP/credential allowlists, and may only
  **subtract** (an `allow_servers` request is intersected with what the parent has
  loaded; the credential allowlist is the parent's, copied). A per-child model is
  resolved **host-side** like the phone-a-friend reviewer, so credentials never
  enter the sandbox or model context.
- **Hard budget split.** The child's cost/token ceiling is **capped at a fraction
  of the parent's remaining budget** (`FLEET_SUBAGENTS_BUDGET_FRACTION`, default
  `0.10` = the #264 "≤10% of remaining per child") and **sliced from what the
  parent has left**, and the child's actual spend is **charged back** into the
  parent. A request for `max_cost_usd`/`max_total_tokens` **above** the per-child
  cap is **refused** (not silently clamped). The parent's configured
  `MaxCostUSD`/`MaxTotalTokens` is the hard wall the collective spend of all
  descendants can never breach.
- **One-level delegation.** `FLEET_SUBAGENTS_MAX_DEPTH` (default `1`) means
  **parent → sub-agent only**: a child does not get the `spawn_subagent` tool
  registered at all, so it cannot delegate further. An operator can raise the depth
  to allow deeper trees.
- **Fan-out cap.** `FLEET_SUBAGENTS_MAX_CHILDREN` (default `5`) bounds per-parent
  fan-out; the `(N+1)`-th spawn is **refused** with `"max concurrent sub-agents
  reached"` as an error **result** rather than blocking.

**Parallel fan-out (#264).** The tool is marked **parallel**, so when the model
emits several `spawn_subagent` calls in one turn, fantasy dispatches them
**concurrently** (bounded by its parallel-tool semaphore) and the parent collects
all results before its next LLM call. The result is **machine-parseable JSON**
`{result, cost_usd, tokens, success}` so the parent can branch deterministically
even when several children return at once. The budget split combines an **atomic
up-front reservation** of each child's granted ceiling (held against the parent's
remaining budget under the parent mutex for as long as the child runs) with
**charge-back** of the child's actual spend on return — so even N **concurrent**
spawns can never collectively be granted more than the parent has left (a
concurrency regression test pins this under `-race`; the wall-clock test pins that
fan-out actually runs in parallel). An optional per-child `timeout_minutes` bounds
a child's wall-clock (spend is still charged back on timeout, `success=false`), and
`max_iterations` caps its agent steps (clamped at the parent's). A spawned child's
run is linked back to its owning task via `parent_task_id` (on the child's session
log and a `subagent_spawned` entry in the parent's persisted log) for traceability.
`FLEET_SUBAGENTS_MODEL` names a default child model slug; empty means the child
inherits the parent's model.

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
with `fleet worktree prune --older-than <dur>` (and `fleet worktree
list` shows all registered worktrees).
