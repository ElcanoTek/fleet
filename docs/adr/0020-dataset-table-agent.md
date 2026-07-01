# ADR-0020: Dataset / table agent rides the governed interactive turn

- **Status:** Accepted
- **Date:** 2026-07-01
- **Deciders:** fleet maintainers

## Context

Issue #514 asks for the "1000-row agent": a table where an agent works each
row toward a goal and writes results back. The triage comments scope the MVP
to *repeatable, reviewable row processing* — typed columns, structured-only
write-back, a review queue, concurrency/pause controls — and flag placeholder
substitution as an injection surface. fleet already has two governed
entrypoints (interactive `Manager.RunTurn`, scheduled `Agent.Execute`) and a
hard invariant: no new run loop.

## Decision

1. **Each row is one governed interactive turn.** The dataset runner
   (`internal/datasets`) dispatches every row through `Manager.RunTurn →
   agentcore.Run` — the same reuse the eval harness (#502) established — so
   rows inherit the sandbox, ceilings, and redaction with zero bespoke run
   path. A worker pool (per-dataset concurrency 1..8) loops on an atomic
   DB claim (`FOR UPDATE SKIP LOCKED`), so workers never double-claim.
2. **Write-back is structured-output only, behind a human gate.** Output
   columns derive a strict schema (`internal/structuredoutput`); a
   non-conforming answer becomes a row NOTE and the row fails. Conforming
   answers land as `proposed` JSONB — cells mutate only on human approval
   (`cells || proposed`, per-row or bulk).
3. **Row values are untrusted data.** The per-row prompt embeds input cells
   as a compact JSON object labeled untrusted; values are never interpolated
   into instruction text (JSON string quoting is the sanitization). The goal
   text is operator-authored and stays outside the data region.
4. **State lives in the sched DB** (migration 047: `datasets` +
   `dataset_rows`), run state is DB-guarded (idle|running|paused transitions
   with a boot sweep for crash-orphaned runs), and the HTTP surface is
   orchestrator handlers with the runner injected via a seam
   (`SetDatasetRunner`) — handlers stay decoupled from the agent graph.

## Consequences

- Per-row cost is bounded by the global per-turn ceilings; a dataset-level
  aggregate budget is a follow-on (per-row cost is recorded).
- Rows run with the native tool set (no MCP selection yet) — the safe
  less-access default, consistent with eval replays.
- A paused/crashed run resumes cleanly: pending rows are re-claimable, and a
  late runner write against a reset/approved row is rejected by a
  status-guarded update.

## Alternatives rejected

- **One scheduled task per row** — reuses task machinery but floods the tasks
  table, loses the review-queue shape, and couples row lifecycle to task
  lifecycle (retention, DLQ, priorities) that doesn't fit cells.
- **Free-form write-back with parsing heuristics** — the triage is explicit:
  structured output or it's a note. Heuristic cell mutation is how tables rot.
- **A generic spreadsheet UI** — out of scope; the table exists to run and
  review agent work, not to be Excel.
