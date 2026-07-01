# Self-improving memory: feedback → learned instructions

A scheduled task "gets better from feedback": thumbs-down + critique on its
runs distill into a **versioned, revertible learned instruction** injected at
run time (#516). This is the next layer on the shipped Captain's Log task
memory (#285) and coordinates with typed memory (#515) and the eval harness
(#502). Off by default (`FLEET_SELF_IMPROVE_ENABLED`).

## The loop

1. **Feedback** — `POST /tasks/{id}/feedback {rating, critique}` records an
   `up`/`down` signal (raw, in `task_feedback`). Anyone who can view the task
   can leave feedback; the Operations Center log modal has thumbs + a critique
   box.
2. **Distillation (staged)** — when fresh `down` signals cross a threshold
   (3) and self-improvement is enabled, a cheap host-side model distills the
   critiques into ONE standing instruction (the SuggestTitle pattern) and
   stages it as **`proposed`** — off-thread, best-effort. A proposal does
   **not** change behavior; the evidence signals are marked consumed so the
   next distillation only sees fresh feedback.
3. **Activation (human, revertible)** — `POST
   /tasks/{id}/learned-instructions/{version}/activate` makes a version the
   task's sole `active` instruction (archiving any prior active). Reverting is
   activating an older version; `DELETE .../learned-instructions/active`
   removes it entirely. Activation requires operator (cancel) permission.
4. **Injection** — a scheduled run injects the active instruction as a
   `## Learned Instruction` prompt section (distinct from persistent memory
   and admin notes). Deactivating removes the section on the next run.

Every instruction is versioned, links the `signal_count` that produced it, and
records who/when it was activated — the auditable, revertible loop the triage
asked for.

## Guardrails / honest scope

- **Enterprise-staged by default**: distillation only *proposes*; a human
  activates. Nothing an agent or a poisoned critique produces changes a run
  until a human clicks Activate.
- **Never writes the client bundle or agent-authored skills** — learned
  instructions are DB runtime state, scoped to the task.
- **Off by default**; feedback is always recorded, but distillation +
  injection require `FLEET_SELF_IMPROVE_ENABLED=true`.
- **Deferred** (documented follow-ons, per the issue's "consolidation" half):
  the background memory-consolidation/dedup loop and skill-abstraction; project
  scoping of learned instructions; and **eval-gated promotion** — pairing an
  activation with a #502 before/after replay so a learned instruction is
  proven to help, not hurt. The hooks (eval harness, per-task feedback) are in
  place; the automated gate is the next step.
