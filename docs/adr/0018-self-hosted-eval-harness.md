# ADR-0018: Self-hosted eval & regression harness

- **Status:** Accepted
- **Date:** 2026-07-01
- **Deciders:** fleet maintainers

## Context

fleet's pitch is "any model, your bundle" — but swapping the core model or
editing a persona/bundle was a blind edit: nothing detected that a class of
tasks silently got worse. Every "eval" in the repo was loop-exit machinery
(#179's exit conditions, the end-of-run verifier), not quality measurement.
Hosted eval platforms (Braintrust, Arize Phoenix, LangSmith) exist, but they
grade YOUR data on THEIR infrastructure — a structural non-starter for the
data-sensitive, self-hosted deployments fleet targets (issue #502).

## Decision

Ship a self-hosted harness with four parts:

1. **Goldens are client content.** Eval sets live in the external bundle's
   `evals/` dir (one YAML per set), extending the ADR-0006 bundle contract
   exactly like `skills/`. `fleet eval capture` turns a past run (scheduled
   task or chat conversation) into a golden case.
2. **Replay through the one governed loop.** `fleet eval run <set>` replays
   each golden via `agent.Manager.RunTurn → agentcore.Run` at the case's
   pinned MODEL — inheriting the mandatory sandbox, ceilings, and redaction
   (ADR-0001/0002) with no bespoke run path. The persona/prompt CONTENT is
   deliberately resolved from the live bundle at replay time: a bundle edit is
   exactly what the eval detects; the pinned model is what a swap compares.
3. **Deterministic scorers + a schema-validated judge.** The loop's exit
   condition scorers were extracted to the shared `internal/scorers`
   (regex/shell/contains/equals, label contract preserved); the LLM-judge
   (`internal/evals/judge.go`) grades against a rubric on the operator's own
   model through the same host-side resolver, its verdict validated against a
   fixed schema via `internal/structuredoutput` with ONE corrective retry, and
   any judge failure FAILS the scorer (fail-closed — a gate must not pass on a
   grade it never got). Graded data and rubrics never leave the box.
4. **Results are operational data.** One immutable `eval_runs` row per run
   (sched migration 046) with per-case JSONB detail and a bundle content
   fingerprint, so regressions are comparable across runs and bundle edits.
   The set's `threshold` (pass fraction) is the CI gate: exit 0/1.

## Consequences

- A bundle repo's CI can gate merges: check out the bundle, `fleet eval run
  <set> --bundle-path . --no-db`, fail on exit 1. fleet's own CI only guards
  the harness itself (unit tests + the shipped example set) — it cannot see
  client bundles (ADR-0006).
- Replays cost real model spend; sets should stay curated (tens of cases, not
  thousands). The eval_runs history + bundle fingerprint make "did this bundle
  edit regress?" a query, not a guess.
- Deferred (fast-follows, documented in docs/EVALS.md): online scoring of
  sampled production runs, replay caching keyed on bundle-sha, MCP tools in
  replays, per-case temperature/network knobs, a web report tab, and canary
  runs comparing candidate configs on live traffic.

## Alternatives rejected

- **Hosted eval platforms** — graded transcripts and rubrics leave the box;
  the self-hosted grader is the point.
- **Judge as a full `agentcore.Run`** — the judge is a single bounded
  tool-less completion; the repo's uniform pattern for those (SuggestTitle,
  AnalyzeTaskFailure, the loop verifier, phone-a-friend) is a short-lived
  agent governed by construction. The REPLAY is the governed run.
- **Goldens in the orchestrator DB** — definitions belong in the versioned,
  operator-reviewed bundle (one review path for client content); the DB holds
  only results.
