# ask / notify + paused-awaiting-human run state

Two human-in-the-loop message types for **scheduled** runs (#510):

- **`notify`** — a non-blocking progress update. The run continues; the message
  is delivered out-of-band (the notifier, #208).
- **`ask`** — a blocking question. The run **ends and releases its sandbox +
  lease**, the task parks in the new non-terminal `paused_awaiting_input`
  status, and a human answer re-queues it — the next run is given the Q&A and
  continues.

## Why ask ends the run (not "hold and wait")

A paused task must **not hold a sandbox/container while waiting for a human**
(the enterprise criterion). So `ask` doesn't block a live run: it records the
question, releases everything, and the task resumes as a fresh run once
answered. "Cost/step ceilings continue from the paused state" in the sense that
accumulated cost persists across the paused→resumed runs; the resumed run
starts clean with the answer injected.

## Flow

1. The agent (scheduled) calls `ask {question}`. The tool's run-context handler
   (installed by the runner pool) records the question and cancels **this
   task's** run (the #508 per-task cancel), so the run ends at the governed
   loop's next checkpoint — sandbox + MCP released by the run's defers.
2. The runner's paused branch writes `paused_awaiting_input` + the question
   under the run's lease and then **clears the lease** (no container held),
   persists the partial transcript (lease-free), and fires a "needs your
   answer" notification.
3. `GET /tasks/paused` lists the awaiting-input queue (filterable). A human
   answers with `POST /tasks/{id}/resume {answer}` (operator permission) →
   status `pending`, answer stored, re-queued.
4. The resumed run injects a `## Resumed — Human Answer` prompt section (the
   Q&A) and clears the pending columns once started.

## Timeout / audience (honest scope)

- **Timeout**: a paused task stays paused until answered (it holds no resources,
  so an indefinite wait is safe). An auto-expire sweep is a documented
  follow-on.
- **Audience/permission**: resuming requires the operator (cancel) permission;
  a per-owner/project-scoped "who may answer" model is a follow-on (coordinates
  with #509 projects + #292 notifications).
- **notify** is out-of-band via the existing notifier; a per-run in-UI timeline
  event class (unifying with #508's live stream) is a follow-on.
- Interactive chat already has a human in the loop, so ask/notify are
  **scheduled-only** (interactive `ask` returns a clear `ASK_UNAVAILABLE`).
