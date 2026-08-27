# Prime Agent comparison — what fleet borrowed, and what it deliberately didn't

**Status:** shipped (issue #990). This page is both the design note for the
features that landed and the comparison write-up the issue asked for.

[Prime Agent](https://github.com/PrimeIntellect-ai/prime-agent) is Prime
Intellect's open-source "self-improving RLM harness": a TypeScript coding/
research agent built around a persistent Python REPL (the model's single tool),
recursive subagents, durable "continual harness" state the agent refines over
time, and daemon-backed long-running sessions. Issue #990 asked for a real
diff against fleet and for borrowing whatever clears the bar — high value, not
borrowing for its own sake.

## The comparison in one paragraph

The two projects answer different questions. Prime Agent optimizes a **single
trusted user's** long-running local sessions — its docs say plainly that its
worker/kernel processes are *not* a security sandbox — and it gives the model
maximal latitude: one `ipython` tool, direct writes to its own harness state,
uncapped subagent fan-out. fleet is a governed multi-user platform: mandatory
sandbox, one governed loop, credentials brokered host-side, human approval on
every durable write. Where the feature sets overlap (budgets, subagents,
scheduling, memory, skills), fleet's version is generally the more governed
one — subagents get hard budget slices vs. no fan-out cap at all; scheduling
has leases/retries/DLQ vs. per-session JSON files; memory is human-approved
vs. self-written. What Prime Agent does better is **context hygiene over very
long horizons**: its compaction machinery, its habit of re-announcing
surviving state after a compaction, and its budget wind-down prompts are
genuinely better-engineered than fleet's equivalents were.

## What fleet borrowed (shipped in this change)

All four landed in `internal/agentcore` / `internal/agent`, behind fleet's
existing governance — no invariant was touched, no ADR needed.

1. **Structured, iterative compaction summaries.** Prime Agent's compaction
   uses a fixed section template and, on repeat compactions, an *update*
   prompt with explicit preservation rules instead of re-summarizing from
   scratch. fleet's interactive compaction summarizer
   (`internal/agent/interactive.go`) now does both: the Goal / Constraints /
   Progress / Key Decisions / Next Steps / Critical Context skeleton, an
   update variant selected automatically when the droppable middle contains a
   previous `[context compaction` summary, and an explicit instruction to
   record file paths and variable names because the sandbox workspace and any
   persistent Python session survive the summarization (Prime Agent's
   "kernel persists" summarizer note, adapted). Details in
   [AGENT-RUNTIME.md](AGENT-RUNTIME.md) § Context-window pressure.

2. **Post-compaction plan re-announcement.** After a compaction Prime Agent
   tells the model exactly which Python variables survived. fleet's analogue
   of "state that outlives the transcript" is the `task_tracker` plan: it is
   host-side state the finish gate keeps enforcing, but the compacted history
   could lose it. Both compaction paths now re-insert a bounded
   `[plan state after compaction]` message (at most one live copy) right
   after the summary whenever open items remain
   (`internal/agentcore/engine.go`, `orchestration.go`).

3. **Budget wind-down notice.** Prime Agent's goal machinery injects a
   "budget almost exhausted — do not start new substantive work, wrap up with
   progress/remaining/blockers/next step" prompt near its token budget; its
   own autonomous mode notably *lacks* the same courtesy and just stops. fleet
   had the same asymmetry: ceilings hard-stopped a run with no warning. A new
   `FLEET_BUDGET_WINDDOWN_FRACTION` knob (default 0.8) now injects a
   request-local wind-down notice into every provider call past the soft
   threshold, plus a one-shot `fleet.budget_winddown` event
   (`internal/agentcore/engine.go` `budgetWindDownStep`). Both modes get it;
   in practice it matters most for unattended scheduled runs.

4. **Completion-audit wording.** Prime Agent's goal continuation carries a
   sharp instruction: audit the current state against *every requirement* of
   the objective; intent, partial progress, and a plausible final answer are
   not proof of completion. That wording is now part of the scheduled finish
   enforcement's self-audit nudge (`internal/agentcore/audit.go`).

## What fleet deliberately did NOT borrow, and why

- **The RLM design itself (one `ipython` tool, everything programmatic).**
  Prime Agent's biggest bet collapses all tools into a persistent host-side
  Python interpreter. It conflicts head-on with fleet's architecture: the
  sandbox is mandatory and file I/O routes through the governed FileOp seam
  (ADR-0036); MCP calls are brokered host-side with per-tool gates
  (ADR-0042) — a generic `mcp.call_tool(...)` from inside the sandbox would
  bypass all four gates or require re-implementing them behind a bridge.
  fleet already has the context-economics half of this idea via progressive
  tool disclosure (BM25 `tool_search`/`tool_call`, ADR-0026).
- **Self-writing harness state (`/refine`, `rlm.harness.*`).** Prime Agent
  lets the model create/update durable prompts, memories, and skill specs
  directly, with LLM-judged "evidence" and no human gate. fleet's doctrine is
  human-in-the-loop for durable writes — `propose_note`, `propose_skill`,
  learned instructions, and memory proposals all stage for approval — and
  that is a deliberate multi-user trust boundary, not a missing feature.
  The mechanically good parts of Prime Agent's refinement (before/after
  snapshots, rollback, refinement history re-fed as evidence) are worth
  revisiting if fleet ever builds an approval-gated refinement loop.
- **Agent-to-agent messaging / retained subagents.** Real machinery in Prime
  Agent (family-scoped reach, rate limits, quiescence barriers) and a real
  gap in fleet (delegation is one synchronous round trip). Too large and too
  governance-sensitive to ride along here; if wanted it should be its own
  issue with an ADR.
- **Kernel snapshot/restore (`dill`) and daemon session trees.** fleet's
  answer to continuity is different by design: the loop is deliberately not
  serializable (ADR-0024); chat continuity rides the Postgres turn journal
  and scheduled continuity rides prompt-injected context (`carry_context`,
  `ask`, wake notes). Prime Agent's per-variable pickle snapshots solve a
  problem fleet's per-turn sandboxed REPL mostly doesn't have.
- **Heartbeats, schedules, goals.** fleet's scheduler (cron, leases, retries,
  DLQ, `run_if`, self-wake) already covers this ground more robustly than
  Prime Agent's per-session JSON job files; fleet's per-task prompt plus the
  task-tracker finish gate covers the in-run objective role of `/goal`.
- **Autonomous quality gates (shell commands as finish gates).** fleet's
  loop tasks (`loop_config` with `shell:` / `regex:` / `llm` exit conditions,
  #179) already are this feature.

## Follow-up ideas noted, not shipped

- **Workspace-unchanged loop guard.** Prime Agent refuses to re-run a failed
  quality gate when a git snapshot (status + diff + untracked-file hash)
  shows nothing changed, and tells the model to change something first. The
  same idea would fit fleet's loop tasks: skip the exit-condition re-check
  (or enrich the fed-forward prompt) when the worker pass left the workspace
  byte-identical. Needs a snapshot mechanism that works for non-git
  workspaces; deferred.
- **Approval-gated refinement.** See above — Prime Agent's snapshot/rollback
  bookkeeping is a good template if fleet's learned-instruction pipeline ever
  grows a self-reflection source alongside user feedback.

## Honest scope

- The structured/iterative summary prompts apply to the **interactive**
  summarizer. Scheduled runs keep their existing behavior: compaction stays
  opt-in (`FLEET_SCHEDULED_AUTO_COMPACT`), and without a wired summarizer
  they use the deterministic placeholder — the plan re-announcement, which
  does not depend on the summarizer, works in both modes.
- The plan re-announcement fires only when the run used `task_tracker` and
  open items remain; it re-announces the tracker's last rendering (bounded to
  2 KiB), not a re-render.
- The wind-down notice is request-local: it never enters persisted history,
  so transcripts show the model reacting to it without showing the notice
  itself. The `fleet.budget_winddown` event is the operator-visible record.
