# ADR-0029: Derived memory knowledge graph (no time columns on relations)

- **Status:** Accepted (extends ADR-0019 — stage 3 of its staged plan)
- **Date:** 2026-07-02
- **Deciders:** fleet maintainers

## Context

ADR-0019 shipped typed, provenanced, bi-temporal memory records and deferred
the entity/relation graph + as-of queries to a "Later" stage (#523). The trap
in adding graph tables next to a bi-temporal record store is a SECOND temporal
surface: if relation rows carry their own learned/valid/retired columns, the
two surfaces drift (a memory retired but its edges still "current", a validity
window edited on one side only), and every write path must update both.

## Decision

The graph is DERIVED, PROVENANCE-LINKED data over the existing memories table
(chat migration 030):

1. `memory_entities` (typed nodes, closed Go-normalized type set — the same
   posture as ADR-0019's memory kinds) and `memory_relations` (triples whose
   object is exactly one of another entity / a literal value, CHECK-enforced).
2. **No time columns on relations.** Every relation references its source
   memory (`memory_id`, FK ON DELETE CASCADE); ALL temporal semantics —
   transaction time (learned_at/retired_at) and valid time
   (valid_from/valid_to) — derive through that join at query time. As-of
   queries (`GraphAsOf`, `ListMemoriesAsOf`) filter the JOINed memory on both
   axes; proposals are always excluded. Retirement cascade IS the join filter;
   deletion cascade IS the FK.
3. Extraction is a gated (`FLEET_MEMORY_GRAPH_ENABLED`, default off),
   best-effort, schema-validated cheap-model call fired when a memory becomes
   ACTIVE, injected into httpapi as a seam (`WithMemoryGraphExtractor`, the
   runner.ErrorAnalyzer pattern) so store never imports agent. Re-extraction
   replaces a memory's edges in one transaction (idempotent).

## Consequences

- The graph can never contradict the records: there is one temporal truth,
  and time-travel over either axis needs no graph-side bookkeeping.
- Memories remain the write model; the graph is a lossy, regenerable
  projection. Dropping every graph row loses no user data.
- A relation cannot outlive, outreach, or out-date its source memory.
- Per-edge validity windows NARROWER than the source memory's are not
  representable — such a fact should be (and is, by extraction granularity)
  its own memory record.

## Alternatives rejected

- **Bi-temporal columns on relations** — the drift trap above; also doubles
  the surface the stage-2 supersession guards must protect.
- **Graph as source of truth (records derived)** — inverts ADR-0019's shipped
  model and breaks the approval-gated write path the whole memory design
  rests on.
- **Deterministic auto-conflict rules now** — still explicitly deferred by
  #523's own triage: policy should follow observed pin/retire/supersede
  acceptance rates, not precede them.
