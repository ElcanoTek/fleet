# ADR-0019: Typed, provenanced user memory (staged toward a temporal graph)

- **Status:** Accepted
- **Date:** 2026-07-01
- **Deciders:** fleet maintainers

## Context

User memory was flat, untyped fact strings injected as prompt bullets — no
type, no provenance, no validity window, no way to retire a stale fact except
deleting it. Long-lived agents therefore confidently cite outdated facts, the
#1 trust-killer issue #515 targets. The issue's north star is a full temporal
knowledge graph (typed entities/relationships, bi-temporal validity,
deterministic contradiction retirement, as-of queries); its triage comments
explicitly split that into stages and warn: **do not auto-delete or silently
retire facts until review semantics are proven.**

## Decision

Ship the staged plan, one stage per PR, on the EXISTING `memories` table
(additive columns — no parallel store, no migration of user data):

1. **MVP (chat migration 026):** every memory is typed (`kind`:
   fact/preference/identity/constraint/context, unknown→fact), carries
   provenance (`source`, `origin` tool|auto|manual, `conversation_id` —
   now RETAINED on accept, `learned_at`), a user-editable validity window
   (`valid_from`/`valid_to`), `pinned`, and soft retirement
   (`retired_at`/`retired_by`). Injection excludes retired rows, orders
   pinned-first, and annotates non-fact kinds + validity windows.
   Full manual controls: PATCH partial updates, retire/restore, pin, delete.
2. **Stage 2 (chat migration 027):** contradiction CANDIDATES — the post-turn
   extractor may claim a new fact `supersedes` an existing memory; the claim
   rides the existing Save/Don't-Save approval card and the old fact is
   retired only on human Accept, guarded (content-hash match, not pinned,
   still active, transactional). Nothing is ever retired silently.
3. **Later (deferred, tracked separately):** entity/relation tables, as-of
   queries, team/project scoping (needs #509), automatic retirement policies.

**Time-axis discipline** (the classic bi-temporal trap, avoided now so the
graph can land later without reinterpreting data): `valid_from`/`valid_to` are
VALID time — when the fact is true in the world; `learned_at` is TRANSACTION
time — when fleet recorded it; `retired_at` is also transaction time — when
fleet stopped trusting/injecting it. Retirement is NOT `valid_to`: a retired
fact may still have been true for its whole window.

## Consequences

- "Agents stop citing outdated facts" is delivered by RETIREMENT (retired rows
  never reach the prompt); the kind/validity annotations are explainability
  and a tiebreaker, not staleness control — sold honestly as such.
- Retrieval is explainable end-to-end: every record shows what kind of fact it
  is, who wrote it (origin/source), where (conversation), when it was learned,
  its validity window, and how to revert (restore/delete). Acceptance keeps
  provenance instead of erasing it.
- `retired_by` ↔ `supersedes` form a doubly-linked provenance edge — the seed
  of the future relation graph.
- Existing rows and clients are untouched: old memories become `kind='fact'`,
  the old PATCH body (`{content}`) still works, and proposals without a kind
  default sanely.

## Alternatives rejected

- **A separate graph store (or Neo4j) now** — the issue itself says Postgres;
  a second memory store before review semantics are proven would double every
  write path for speculative value.
- **Deterministic auto-retirement on contradiction now** — explicitly warned
  against by triage; free-text facts make "contradiction" a model judgment,
  and a wrong judgment silently deleting user memory is the trust-killer in
  a different costume. Human-confirmed supersession first.
- **Storing type as free text** — a closed, Go-normalized kind set keeps the
  column machine-filterable (the graph's future entity typing depends on it).
