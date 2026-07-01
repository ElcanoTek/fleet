# ADR-0024: ask releases the sandbox and re-queues; it does not hold a lease

- **Status:** Accepted
- **Date:** 2026-07-01
- **Deciders:** fleet maintainers

## Context

Issue #510 wants a scheduled agent to post non-blocking progress (`notify`) and
a blocking question (`ask`) that pauses the run for a human. The enterprise
criterion (triage): a paused task must be queryable/filterable and **must not
hold a sandbox/container lease indefinitely** while waiting. Truly suspending an
in-flight `agentcore.Run` across a process boundary isn't feasible (the run is
in-memory), and holding a warm container per paused task doesn't scale.

## Decision

Model `ask` as **end-run + re-queue**, reusing the #508 per-task interrupt:

1. The `ask`/`notify` tools are scheduled-only native tools whose handlers are
   installed on the run context by the runner pool (the ctx-seam pattern, like
   `WithArtifactCollector`). Colocated in `internal/tools` so `runner`/
   `scheduledrun` (which import tools) install them with no cycle.
2. `ask` records the question and cancels **this task's** run context (the #508
   `activeRun.cancel`). The run ends at the loop's next checkpoint; the sandbox
   + MCP client are released by the run's existing defers.
3. The runner's paused branch (checked first, like the stop branch) writes
   `paused_awaiting_input` + the question **and clears the lease** — so a paused
   task holds no container — persists the partial transcript (lease-free
   `submitLog`), and fires an out-of-band "needs your answer" notification.
   `paused_awaiting_input` is a NEW non-terminal status (excluded from
   `IsTerminal`, from lease recovery, and from retention).
4. `POST /tasks/{id}/resume {answer}` sets `pending_answer` + status `pending`
   (re-queued); the resumed run injects the Q&A as a prompt section and clears
   the pending columns under its lease. Accumulated cost persists across the
   paused→resumed runs.
5. `notify` fires the notifier (#208, a new `StatusProgress` event) and returns
   immediately — the run continues.

## Alternatives rejected

- **Suspend the live run and hold the sandbox** — violates the no-indefinite-
  lease criterion and can't survive a process restart.
- **Persist and re-hydrate mid-loop agentcore state** — the run loop is
  in-memory and not serializable; re-queue-with-context is simpler and robust.
- **A dedicated `ask` approval-card path in interactive chat** — a human is
  already present there; ask/notify are scheduled-only (interactive `ask`
  returns `ASK_UNAVAILABLE`).
