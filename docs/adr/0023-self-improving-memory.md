# ADR-0023: Self-improving memory is staged feedback→learned-instructions

- **Status:** Accepted
- **Date:** 2026-07-01
- **Deciders:** fleet maintainers

## Context

Issue #516 asks for two idle-time loops: memory consolidation, and
feedback→learned-instructions. The triage reframed it as a layer on the
shipped self-improvement primitives (Captain's Log #285, propose_note, typed
memory #515, eval harness #502) and set the enterprise default: background
review may *propose*, but learned instructions require staged review before
activation, must be versioned/revertible/scoped, linked to evidence, and must
never write the operator-owned bundle or agent skills.

## Decision

Ship the **feedback→learned-instructions** half, staged:

1. **Raw signals** (`task_feedback`) — thumbs + optional critique on a task's
   runs. Recording is always on; distillation is gated.
2. **Distillation is a proposal, not an activation.** At a threshold of fresh
   down-signals, a cheap host-side model (the SuggestTitle pattern, through the
   same resolver — no secrets in the sandbox) distills the critiques into one
   `proposed` instruction, off-thread, best-effort. Evidence is marked consumed
   atomically with the proposal.
3. **Human-activated, versioned, revertible** (`task_learned_instructions`):
   at most one `active` per task (a partial unique index), activation archives
   the prior active, and revert = re-activate an older version (or deactivate).
   Activation requires operator permission and records who/when.
4. **Injected as a distinct prompt section** at scheduled run-start, separate
   from persistent memory (facts) and admin notes (knowledge). Gated on
   `FLEET_SELF_IMPROVE_ENABLED`; a nil provider leaves runs byte-for-byte
   unchanged.

Deferred (documented): the background consolidation/dedup + skill-abstraction
loop, project scoping, and eval-gated promotion (pair activation with a #502
replay) — the hooks exist; the automated gate is the follow-on.

## Alternatives rejected

- **Auto-activate distilled instructions** — violates the enterprise-staged
  mandate; a poisoned critique could silently reshape a run. Propose-then-human
  is the whole point.
- **Writing learned lessons into the client bundle or a skill** — the bundle
  is the operator-owned, reproducible git artifact; learned instructions are
  per-task runtime state.
- **One free-text "current instruction" field** — no version history, no
  revert, no evidence link; the versioned table is the auditable shape the
  triage required.
