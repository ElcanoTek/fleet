# Using other agents in fleet

fleet runs your turns through a selectable **runtime flavor**. Out of the box
that flavor is fleet's own agent loop — but fleet is, under the hood, an
[Agent Client Protocol](https://agentclientprotocol.com) (ACP) **client**, so it
can also drive *other* coding agents (Claude Code, Goose, …) as first-class,
sandboxed flavors you pick per chat or per scheduled task.

This guide is for the person wiring those agents into a deployment. It covers:

- [The flavor model](#the-flavor-model) — the three flavors and when to use each
- [Governance tiers](#governance-tiers-read-this) — what fleet does and does **not**
  guarantee for each, stated honestly (read this before you enable an external agent)
- [Adding an external agent flavor](#adding-an-external-agent-flavor) — the manifest
  entry, the provider image, and the model-cred env vars
- [Picking a flavor](#picking-a-flavor) — per chat and per scheduled task
- [The permission UI](#the-permission-ui) — how an external agent asks the human
- [Worked example: adding Goose](#worked-example-adding-goose)
- [Data-residency caveat](#data-residency-caveat) — the one thing you cannot wave away
- [Drive fleet from your editor over ACP (ingress)](#drive-fleet-from-your-editor-over-acp-ingress)
  — **advanced/optional**: the inverse — an editor drives fleet's own governed
  pipeline. A developer convenience, NOT part of the web product; skip unless you
  specifically want fleet as your editor's agent backend.

Everything here is generic. The manifest snippets are written to be copied
straight into any `XYZ-config` client bundle's `manifest.yaml`.

---

## The flavor model

A **flavor** is one entry under `runtimes:` in your bundle's `manifest.yaml`.
Each has a `type`:

| `type`             | What runs                                                                 | Where tool calls execute              | Trust posture          |
| ------------------ | ------------------------------------------------------------------------- | ------------------------------------- | ---------------------- |
| `native-inprocess` | fleet's own loop, in the fleet process                                    | host (the per-turn sandbox)           | **Fully governed**     |
| `native-acp`       | fleet's own loop, wrapped as a sandboxed ACP agent (`fleet-native-agent`) | host (delegated back over ACP)        | **Fully governed**     |
| `acp` (external)   | a **third-party** agent (Claude Code / Goose) in its own sandbox          | inside the agent's sandbox (self-run) | **Containment** tier   |

- **`native-acp`** is the **default** flavor (#159). It is the exact same loop
  and the exact same governance as `native-inprocess` — the loop just runs inside
  a container and delegates every `bash`/`run_python` call back to the host, where
  fleet applies full policy/audit/notes/credential-injection. fleet is the parent;
  the agent cannot self-execute. Because the loop runs in its own container it
  never shares the fleet process's address space — and thus never the host-held
  MCP credentials (it holds only the model-endpoint key).
- **`native-inprocess`** is the fast path and the parity oracle: today's
  in-process loop, best for dev/test/trusted-local. It is **off by default** —
  re-enable it with `FLEET_ENABLE_INPROCESS_LOOP=1`, which also drops it from the
  selectable catalog and rejects it server-side when unset. ⚠️ Its loop runs *in
  the fleet process*, so it shares an address space with the host-held MCP
  credentials: it is **not** a credential-isolation boundary. Use it only for
  trusted-local/dev/parity; for isolation use the default `native-acp`. (A
  follow-up tracks moving the credential broker out-of-process so even this lane
  delegates credentials — see #159.)
- **`acp` (external)** drives a *different vendor's* agent. That agent
  **self-executes inside its own locked sandbox** — it does not hand its tool
  calls back to fleet. This is a fundamentally different and weaker trust
  posture, and fleet treats it as such (see the next section).

All three are driven over the *same* ACP seam, so they are interchangeable from
the picker's point of view. They are **not** interchangeable from a governance
point of view, and fleet never pretends otherwise.

---

## Governance tiers (read this)

fleet stamps every external turn's tier into the session log (`governance:
delegated`) so the audit trail records, per turn, which posture it ran under.
There are two tiers. We document them honestly and never conflate them.

### Tier 1 — **Fully governed** (`native-inprocess`, `native-acp`)

fleet owns execution end to end:

- **Per-tool-call policy.** Every `bash`/`run_python`/MCP call passes through the
  host policy gate (cost ceilings, repeat detection, critical-tool approval
  staging, the email send gate, …).
- **Audit.** fleet records the real, executed tool calls — not a self-report.
- **Notes & context.** The notes wiki and per-conversation context are injected
  host-side.
- **MCP credentials.** Brokered host-side at the moment of delegation. They are
  **never** placed in any agent container. For `native-acp` this means the agent
  advertises the MCP tool surface but every `mcp_*` call rides `_fleet/mcp` back
  to the host, which runs it against the per-task credentialed client — the
  container holds only the model-endpoint key, never an MCP credential.
- **Approval / memory / note staging.** `native-acp`'s in-loop policy decides
  *when* to stage (identically to in-process); the staging *effect* (the approval
  card, the memory/note proposal) rides `_fleet/stage` back to the host, where the
  real stagers persist it and emit the SSE card.
- **Usage & cost.** The `native-acp` agent makes the LLM calls inside its
  container and reports each step's token/cost over `_fleet/event`; the host
  accumulates it and enforces the same cost/token ceilings (shipped in the run
  spec, enforced in-loop) as in-process. As **defense-in-depth across the
  container boundary**, the host *also* re-checks that reconciled usage against
  the spec ceilings itself and **hard-cancels the run** if a buggy or
  compromised agent overshoots — so the ceiling does not rely solely on the
  in-container soft gate.
- **Blast radius.** Tool calls run in fleet's own hardened per-turn sandbox.
- **Lockdown.** Lockdown bounds *tool execution*, not the model call. Tool calls
  already execute host-side in fleet's per-turn sandbox, so a lockdown turn hands
  `native-acp` a **no-network** host sandbox and the isolation holds exactly as
  in-process. The agent container itself legitimately keeps model-endpoint egress
  to run the LLM loop — the same posture as the in-process server process under
  lockdown, where only the per-turn TOOL sandbox is sealed.

### Tier 2 — **Containment** (`acp` external, `delegated_policy: true`)

The external agent self-executes. fleet cannot enforce per-tool policy on code
it does not run. So instead of *governing*, fleet **contains**:

- **`governance: delegated`** is stamped into the session log for the turn.
- **Audit is the agent's self-report.** fleet captures the agent's
  `session/update` stream (its narrated text, thoughts, and tool-call notices)
  as *observed / audited*, **not enforced**. If the agent under-reports what it
  did, fleet cannot know.
- **Usage/cost is the agent's self-report too.** fleet records the token totals
  the agent reports on its `PromptResponse` and the cumulative cost it reports
  over `session/update` usage notifications (USD only — a non-USD figure is
  logged but not coerced into the dollar field). The external agent drives its
  **own** model endpoint, so fleet does **not** meter it: an unreported cost is
  recorded as an honest **unmetered zero**, never a true `$0`, and a
  containment-tier run is **excluded** from any "cost ceiling satisfied" claim.
- **Locked sandbox.** The provider's agent runs `--read-only`, `--cap-drop=ALL`,
  `--security-opt=no-new-privileges`, with a **scratch-only** tmpfs workspace
  discarded on teardown.
- **Restricted egress.** `network: model_only` — the only network the agent is
  meant to reach is its model endpoint. (fleet stamps the intent and keeps the
  env scrubbed; enforcing `model_only` at the packet level is the host
  firewall's job — see the note in the worked example.)
- **Scrubbed env — no fleet secrets.** The agent container receives **only** the
  provider's own model key (the env var names you declare in `model_env`).
  fleet's secrets and your MCP credentials are never shipped to it.
- **Coordinated teardown.** fleet kills the whole process group + container; it
  never trusts `--rm` alone.
- **Permission gate.** Anything the agent flags as sensitive routes to a human
  (next section), **default-deny on timeout, no "approve all."** On the
  **scheduler** there is no human, so there is no broker at all — every permission
  request is **denied** (see
  [Scheduled-external](#scheduled-external-is-fail-closed-and-off-by-default), below).

> **The honest bottom line:** containment bounds what an external agent can *do
> on your host*. It does **not** stop a self-executing agent from sending what
> it *reads* (your workspace files) to its own model endpoint. See
> [Data-residency caveat](#data-residency-caveat). Use the external tier when the
> convenience of a specific vendor's agent is worth that trade-off — and only
> then.

---

## Adding an external agent flavor

Three things, all in your client bundle:

### 1. A `runtimes:` manifest entry

Add an entry of `type: acp` to `manifest.yaml`:

```yaml
runtimes:
  # … keep your native flavors …

  my-agent:                                   # the flavor key (shown in the picker)
    type: acp
    image: "ghcr.io/acme/my-agent@sha256:…"   # the provider sandbox image (DIGEST-PINNED)
    network: model_only                       # restrict egress to the model endpoint
    delegated_policy: true                    # the containment tier (governance: delegated)
    model_env: ["ACME_API_KEY"]               # env var NAME(S) carrying the provider's OWN model key
    args: ["acp"]                             # optional: extra args to the agent entrypoint
    display_name: "My Agent (external)"
    description: "Acme's agent over ACP. Self-executing; containment tier."
    beta: true
```

Field notes:

- **`image`** — pin by digest in production. This is the provider's agent baked
  into a container that speaks ACP over stdio (stdout = protocol, stderr =
  diagnostics; no PTY).
- **`network: model_only`** — the egress posture. fleet stamps the intent and
  keeps the env scrubbed regardless; pair it with a host egress policy that
  actually allows only the model endpoint.
- **`delegated_policy: true`** — marks the containment tier. Without it the
  flavor would not be treated as an external self-executing agent.
- **`model_env`** — the *names* of the env vars holding the provider's own model
  credential. At spawn, fleet reads these from **its own** environment and
  passes **only** them into the agent container. (So you put `ACME_API_KEY=…`
  in fleet's env file; fleet forwards it, and nothing else, into the sandbox.)
- **`args`** — appended to the container entrypoint. Goose, for instance, needs
  `["acp"]` to start its ACP server.

### 2. The provider sandbox image

Build (or pull) a container that runs the provider's agent as an ACP server over
stdio. The canonical patterns:

- **Claude Code** → the [`claude-agent-acp`
  bridge](https://github.com/zed-industries/claude-code-acp) (a.k.a.
  `@zed-industries/claude-code-acp`) wraps the Claude Code CLI as an ACP agent.
  Bake the bridge + the CLI into the image; the bridge reads `ANTHROPIC_API_KEY`.
- **Goose** → speaks ACP natively. The entrypoint is `goose acp` (hence
  `args: ["acp"]`). Bake the `goose` binary into the image.

Pin the base by digest and keep the image minimal — the agent self-executes
nothing fleet governs, so it only needs its own runtime.

> The generic `config/default` bundle ships **documented placeholder** entries
> for `claude-code` and `goose` (with `localhost/...` image refs) so the wiring
> is visible and testable. A real `XYZ-config` bundle overrides them with its own
> digest-pinned images and its own model creds.

### 3. The model-cred env vars

Put the provider's model key in fleet's env file (the same env file that holds
the rest of fleet's config), using the variable name(s) you listed in
`model_env`:

```sh
# fleet's env file
ACME_API_KEY=sk-acme-…
```

That key is the **only** secret that ever reaches the external agent's
container.

---

## Picking a flavor

### Per chat

The chat composer shows a **runtime picker** whenever the bundle defines more
than one flavor. Click it, choose a flavor; the choice is stored on the
conversation (`POST /conversations/{id}/runtime`) and applies to that
conversation's next turn. An empty/cleared choice falls back to the bundle's
`default:` flavor.

### Per scheduled task

Scheduled work selects its flavor **process-wide** via `FLEET_SCHEDULED_RUNTIME`
(resolved at boot against the bundle's `runtimes:` catalog; unset / unknown falls
back to `native-inprocess`). A scheduled `native-acp` task runs the SAME scheduled
loop (audit gating, finish enforcement) through the sandboxed agent, fully
governed host-side — MCP credentials are brokered over `_fleet/mcp` and never
enter the container, `propose_note` staging rides `_fleet/stage`, and usage/cost
is reconciled over `_fleet/event`. It governs identically to a scheduled
`native-inprocess` task.

A bundle that wants to keep scheduled work fully governed simply does not point
`FLEET_SCHEDULED_RUNTIME` at an external flavor — external scheduled runs remain
the containment tier and are opt-in (see the next section).

#### Scheduled execution sandboxes are network-SEALED by default

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

#### Scheduled-external is FAIL-CLOSED and off by default

Pointing `FLEET_SCHEDULED_RUNTIME` at an **external** (`type: acp` /
`delegated_policy: true`) flavor runs a third-party agent **on the scheduler,
with no human watching it**. That is a deliberately gated capability, off by
default, and **fail-closed** — the exact OPPOSITE of the `native-acp` fallback:

- **Per-client opt-in.** A scheduled-external run is admitted ONLY when the
  client manifest sets

  ```yaml
  agent_policy:
    allow_ungoverned_scheduled_agents: true   # default: false (the generic bundle leaves it unset)
  ```

- **Off → a LOUD ERROR at dispatch, never a fallback.** With the flag off (the
  default), a scheduled task that selects an external flavor **fails at dispatch**
  with a clear governance message recorded in the run/session log. fleet does
  **not** silently fall back to a native flavor — doing so would run a *different*
  agent than the operator chose. An under-governed external agent never runs
  unless you explicitly opt in. (Contrast: `native-acp` *may* fall back to the
  in-process loop, because that runs the SAME agent under STRONGER governance.)
- **On → containment tier, sandbox REQUIRED.** With the flag on, the scheduled
  turn runs through the **same** `acpruntime.ExternalRuntime` the interactive path
  uses, at the **containment** tier: `governance: delegated` is stamped in the run
  record, the agent self-executes in its locked provider sandbox, and the env is
  scrubbed to the `model_env` key only. The sandbox image is **mandatory** — a
  scheduled-external flavor with no image is an error, not a degraded run.
- **No human on the loop → default-DENY permissions.** Scheduled work has no
  approver, so fleet wires **no** permission broker. Every
  `session/request_permission` the external agent raises is **denied** (the same
  fail-closed deny a misconfigured interactive flavor gets — no approve-all, no
  hang). A scheduled-external task that needs approval simply cannot take that
  action, by design.

> **Honest framing:** containment is **not** full governance. A scheduled-external
> run is the same containment posture as interactive-external (the agent may
> transmit your workspace to its own model endpoint — see the
> [Data-residency caveat](#data-residency-caveat)), minus the human approval gate.
> Keep `allow_ungoverned_scheduled_agents` off unless a specific vendor agent's
> convenience is worth running it unattended.

The **end-of-run verifier** — an extra host-side LLM re-check the scheduled
driver layers on top of the shared audit/finish enforcement — now runs for
scheduled `native-acp` too, over the `_fleet/verify` delegation seam: when the
agent's in-loop scheduled policy clears, it asks the host to verify, shipping the
tool-exec summary it authoritatively holds; the host runs the **same**
`runEndOfRunVerifier` on its **own** fallback model (host-side creds — the
verifier's model call never enters the agent container) and returns any missing
required actions, which the agent turns into a final enforcement round. A
host-side verifier error fails **open** (allow finish), exactly as the in-process
path does. So core governance — per-tool policy, audit, finish enforcement, MCP
credential brokering, note staging, usage/cost, **and the end-of-run verifier** —
is at full parity; native-acp never silently finishes a scheduled run unverified.

### Context-window pressure (proactive compaction)

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
the carried-over history so a single-round interactive turn that *starts* near
the limit is still covered.

**Scheduled safeguard.** Unattended runs must not silently rewrite their own
transcript: in `ModeScheduled` the warn event still fires (and a breadcrumb is
written to the session log), but proactive compaction is **off** unless the
operator sets `FLEET_SCHEDULED_AUTO_COMPACT=1`. The summary uses the driver's
`compactionSummarizer` (an LLM summary) when wired, else a deterministic
placeholder — the same hook the reactive `context_length_exceeded` recovery path
already uses, so a proactive compaction does not count toward the consecutive-
compaction cap that guards against compaction loops.

### Per-task credential allowlist (least-privilege MCP)

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

### Git worktree isolation

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
exclude). The run is scoped into the worktree by two complementary host-side seams:
the per-run sandbox's default working directory (which the `native-acp` default
flavor's host-delegated bash/run_python calls inherit) and a per-run forced
working directory threaded into the in-process tool layer. Together they scope
**bash, run_python, and the relative-path file tools** into the worktree on both
the in-process and `native-acp` paths — git operations (driven through bash, the
point of the feature) are isolated on the production default flavor. The one
uncovered path is an agent that writes through the ACP-native `fs/*` capability
*directly* (a fallback the native agent does not use — it routes file ops through
bash/run_python); those writes are not redirected into the worktree.

**Cleanup.** With `auto_cleanup: true` the worktree and its branch are removed
after the run (optionally after `cleanup_delay_seconds`); with `false` the branch
is left in place for inspection or a manual push. Orphans from a crashed run (the
process died between worktree creation and cleanup) are reclaimed by an operator
with `fleet-admin worktree prune --older-than <dur>` (and `fleet-admin worktree
list` shows all registered worktrees).

---

## The permission UI

When an external agent decides an action is sensitive, it sends
`session/request_permission` over ACP. fleet routes that to **a human**:

1. fleet emits a `permission.requested` event over SSE. The agent's turn
   **blocks** server-side, waiting.
2. The chat UI renders an inline **allow / deny** card showing what the agent
   wants to do — the title, the affected file paths, and the agent's offered
   options.
3. The user clicks **Allow** (picking one of the agent's allow-shaped options)
   or **Deny**. The decision POSTs to
   `/conversations/{id}/permissions/{requestId}`, the agent's turn unblocks, and
   it proceeds or skips accordingly.

Two safety properties are non-negotiable and enforced server-side:

- **Default-deny on timeout.** If no human answers within the window, the
  request is **denied**, never allowed. No human, no allow.
- **No "approve all."** Every request is decided on its own merits. fleet does
  not surface a one-click blanket approval, even if the agent offers an
  `allow_always` option (it is treated as a one-time allow).

A misconfigured external flavor with no permission broker **fails closed** —
every request is denied.

---

## Worked example: adding Goose

[Goose](https://github.com/block/goose) speaks ACP natively, which makes it a
clean example.

**1. Build the image.** A minimal container with the `goose` binary, entrypoint
left as the binary so `args: ["acp"]` starts the ACP server:

```Dockerfile
FROM debian:stable-slim@sha256:…           # pin by digest
RUN install-goose.sh                       # however you obtain the binary
ENTRYPOINT ["goose"]
```

Build and (optionally) push it:

```sh
podman build -t ghcr.io/acme/fleet-goose:latest -f Containerfile.goose .
```

**2. Add the flavor** to your bundle's `manifest.yaml`:

```yaml
runtimes:
  goose:
    type: acp
    image: "ghcr.io/acme/fleet-goose@sha256:…"   # the digest you just built
    network: model_only
    delegated_policy: true
    model_env: ["OPENROUTER_API_KEY"]            # Goose-on-OpenRouter; use your provider's key name
    args: ["acp"]
    display_name: "Goose (external)"
    description: "Block's Goose via native ACP. Self-executing; containment tier."
    beta: true
```

**3. Supply the model key** in fleet's env file:

```sh
OPENROUTER_API_KEY=sk-or-…
```

**4. Restrict egress.** Pair `network: model_only` with a host firewall/egress
rule that allows only your model endpoint from the Goose container's network.
fleet keeps the container env scrubbed regardless, but the packet-level
restriction is the host's responsibility — `network: model_only` is the
*declaration*, your firewall is the *enforcement*.

**5. Pick it.** Reload fleet (so it re-reads the bundle), open a chat, choose
**Goose (external)** in the runtime picker, and send a turn. Watch for the
permission card the first time Goose tries something sensitive.

That's it. Claude Code is identical except you bake the
[`claude-agent-acp`](https://github.com/zed-industries/claude-code-acp) bridge
into the image and set `model_env: ["ANTHROPIC_API_KEY"]`.

---

## Data-residency caveat

This is the one thing you must not wave away when enabling an external flavor.

An external agent runs your turn against **its own** model endpoint. To do useful
work it reads your workspace — the files, data, and context of the task — and
those reads go into the prompts it sends to its provider. **Containment cannot
prevent this**: it bounds what the agent can *do on your host*, not what it can
*transmit about what it reads*.

So: an external flavor sends your workspace contents to that vendor's model
endpoint. If your data has residency, confidentiality, or contractual
constraints, only enable external flavors whose provider you are contractually
comfortable sending that data to — and prefer the **fully-governed** native
flavors (`native-inprocess` / `native-acp`) for anything sensitive. fleet stamps
this caveat into the session log alongside every external turn so the record is
unambiguous.

### Exactly what an external agent can reach

- **Interactive turns:** the conversation's workspace is bind-mounted at
  `/workspace` **read-only**. The agent can **read** the files the user uploaded
  or the conversation accumulated (which is what makes it useful, and what the
  caveat above is about), but it **cannot write to the host** — its scratch
  writes go to an ephemeral in-container `/tmp` tmpfs discarded on teardown.
  Persisting outputs to the durable workspace remains a fully-governed
  (`native-*`) job.
- **Scheduled-external turns:** **scratch-only** — `/workspace` is an empty
  writable tmpfs and the agent sees only the task prompt text, never the host
  workspace. An unattended, human-less run gets the most conservative posture by
  design (on top of the per-client opt-in and fail-closed permissions above).

Either way the rest of the containment hardening is unchanged: read-only rootfs,
`--cap-drop=ALL`, `no-new-privileges`, env scrubbed to the `model_env` key only,
and the declared egress posture.

---

## Drive fleet from your editor over ACP (ingress)

> **Advanced / optional — most deployments never use this.** Ingress is a
> developer convenience, not part of the web chat/orchestrator product your users
> log into. It is useful only to a developer who wants fleet's *governed* tool
> surface (host-brokered MCP credentials, sandbox, personas, notes, cost ceilings)
> inside their own editor, co-located with a fleet deployment. If that's not you,
> skip this section — nothing else depends on it.

Everything above is fleet driving *other* agents. Ingress is the **inverse**:
fleet exposes **itself** as an ACP agent so an editor (Zed, Neovim, any
ACP-speaking host) can launch it and drive fleet's **own** governed, sandboxed
pipeline — your editor becomes the chat surface, and fleet's policy, sandbox, MCP
catalog, notes, audit, and cost ceilings all still apply.

You start it with the `acp` subcommand:

```bash
fleet acp
```

It speaks ACP over **stdio** (JSON-RPC on stdin/stdout, logs on stderr — no PTY),
which is exactly how editors spawn ACP agents.

### Configure your editor

`fleet acp` is just an ACP agent command. Point your editor's ACP agent config at
it and pass the same environment fleet's server uses (the OpenRouter key, the
client-config bundle dir, the database URLs).

**Zed** — in `settings.json`:

```json
{
  "agent_servers": {
    "fleet": {
      "command": "/usr/local/bin/fleet",
      "args": ["acp"],
      "env": {
        "OPENROUTER_API_KEY": "sk-or-...",
        "FLEET_ACP_MODEL": "anthropic/claude-opus-4.8",
        "FLEET_CLIENT_CONFIG_DIR": "/etc/fleet/config",
        "FLEET_CHAT_DATABASE_URL": "postgres://.../fleet_chat",
        "FLEET_SCHED_DATABASE_URL": "postgres://.../fleet_sched"
      }
    }
  }
}
```

**Neovim** (any ACP plugin, e.g. CodeCompanion's ACP adapter) — configure an agent
whose command is `fleet acp` with the same `env`. The exact key names vary by
plugin; the command + env are what matter.

Ingress-specific environment knobs:

| Env var                | Meaning                                                                 |
| ---------------------- | ----------------------------------------------------------------------- |
| `FLEET_ACP_MODEL`      | **Required.** The OpenRouter slug ingress turns drive (e.g. `anthropic/claude-opus-4.8`). Falls back to `LLM_DEFAULT_MODEL`. |
| `FLEET_ACP_RUNTIME`    | Flavor for ingress turns (`native-inprocess` / `native-acp`). Defaults to the bundle's default flavor. |
| `FLEET_ACP_PERSONA`    | Persona for ingress turns. Defaults to the bundle's default persona.    |
| `FLEET_ACP_PRINCIPAL`  | The audit identity ingress sessions attribute to (see the trust model). Defaults to a placeholder. |
| `FLEET_ACP_LOCKDOWN`   | Opt this `fleet acp` process into lockdown: every ingress turn runs in the sealed, no-network per-turn sandbox. ORed with the server's `CHAT_LOCKDOWN_ONLY`, so a `LockdownOnly` server always seals ingress turns regardless of this flag. Requires a configured sandbox image and a model on `CHAT_LOCKDOWN_ALLOWED_MODELS` — validated at startup. |

### Trust model (read this)

Launching `fleet acp` runs **as the box user** — it is the same trust as running
`fleet` itself. This is **local-process trust**, distinct from the web path's
signed-key auth: whoever can start the process can drive a governed turn. So:

- Only configure `fleet acp` on a machine where the operator already has the
  same trust the fleet process has.
- Ingress sessions bind to a configured **principal** (`FLEET_ACP_PRINCIPAL`, an
  operator/service identity) so the conversation + approval rows attribute
  correctly in the audit trail. It is **not** an authentication credential — it
  only names who the local operator is acting as.
- **Remote ingress is out of scope.** `fleet acp` is a local stdio adapter; it
  does not listen on a socket and is not meant to be exposed over a network.

### What is and isn't supported

| Supported                                                          | Not supported (yet)                                  |
| ------------------------------------------------------------------ | ---------------------------------------------------- |
| Interactive streaming turns (`session/update` text + tool calls)   | Scheduled tasks (use the orchestrator for those)     |
| Human approval over `session/request_permission` (default-**deny** on timeout / cancel / no answer — **no approve-all**) | Remote / networked ingress |
| Full governance: fleet's policy, sandbox, MCP catalog, notes, audit, cost ceilings | Remote / networked ingress (listed above)            |
| `loadSession` / `resume`: reconnect to a prior conversation across `fleet acp` restarts (the SessionId **is** the conversation id) — `loadSession` replays the persisted transcript, `resume` rebinds without replay | Audio prompt blocks |
| Whatever runtime flavor the box runs (`native-inprocess` / `native-acp` sandbox); image prompt blocks (decoded to the workspace + fed to the turn as vision) | |
| `propose_note` (wired host-side on the Manager) + `propose_memory` (staged, then confirmed over `request_permission`, default-DENY); email `content_file` inlining + relative-attachment-path materialization (same host-side transform as the web path) | |

### fleet uses its OWN sandbox + MCP catalog (host MCP passthrough is unsupported)

This is a deliberate governance choice, not a gap. The ACP `session/new` request
lets a host advertise its own `mcpServers`. **fleet ignores them.** fleet brokers
its **own** client-config MCP catalog host-side, with credentials that never
leave the host — so accepting the editor's MCP endpoints would open an
un-governed credential path that bypasses exactly the brokering ingress is built
to preserve. Likewise, tool execution always happens in **fleet's** sandbox under
**fleet's** policy, never in the editor. If you need a tool available to an
ingress turn, add it to fleet's MCP catalog, not your editor's.

The human-approval surface is the one piece that *does* cross to the editor: when
fleet's policy stages a critical action (a risky `bash`, an outbound email), the
ingress agent sends an outbound `session/request_permission` and your editor
prompts you. Approve, and fleet executes the staged action through the **same**
governed out-of-band path the web approval card uses. Deny — or simply don't
answer before the timeout — and the action is **not** taken.

---

## How it works (for the curious)

- fleet is an ACP **client** (`internal/acpruntime`). For an external flavor it
  spawns the provider's image via `podman run -i`, drives
  `Initialize → NewSession → Prompt`, and implements `acp.Client`.
- The external client (`internal/acpruntime/external.go`) **forwards** the
  agent's `session/update` stream onto fleet's `Observer` (→ SSE / session log) —
  that self-report is the containment-tier audit — and routes
  `session/request_permission` to the `PermissionBroker` (the human, default-deny).
- The native flavors instead delegate `bash`/`run_python` back to the host over
  the `_fleet/*` ACP extension methods, where fleet's real `agentcore.Executor`
  runs them under full policy. That is what makes them *governed* rather than
  merely *contained*.

The deterministic test provider for the external path is the credential-free ACP
example agent in `cmd/acp-example-agent` (a turn streams, a permission request is
handled) — proving the generic external path end-to-end without any provider
credentials. Claude Code and Goose are wired the same way.

- **Ingress** (`internal/acpingress`) is the inverse: fleet is the ACP **agent**.
  `fleet acp` serves `acp.Agent` over stdio; each `Prompt` runs the **same**
  governed interactive turn the web path runs (`agent.Manager.RunTurn` →
  `agentcore.Run`), only swapping the I/O surfaces — the prompt comes from an ACP
  `PromptRequest`, streamed output goes out as `session/update`, and a staged
  critical-tool approval goes out as `session/request_permission` (default-deny).
  It is an I/O adapter on the existing governed turn, **not** a second governance
  path: the policy, sandbox, MCP brokering, notes, audit, and cost ceilings are
  all the web path's, inherited verbatim.
