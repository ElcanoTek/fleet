# User memory: typed, provenanced, reviewable

fleet's user memory is a set of per-user records injected into every chat turn
(the `## User Memories` prompt section). Since #515 they are TYPED records with
full provenance and lifecycle controls — not just fact strings. (ADR-0019.)

## The record

| field | meaning |
|---|---|
| `content` | the fact text (≤4000 runes) |
| `kind` | `fact` \| `preference` \| `identity` \| `constraint` \| `context` (unknown values normalize to `fact`) |
| `source` | `manual` (typed in the manager) \| `chat` (accepted proposal) \| `proposed` (pending review) |
| `origin` | who wrote it: `manual`, `tool` (the agent's `propose_memory`), `auto` (the post-turn extractor #234) |
| `conversation_id` | where it came from — **retained after acceptance** (provenance) |
| `learned_at` | when fleet recorded it (transaction time) |
| `valid_from` / `valid_to` | when the fact is true in the world (valid time; user-editable, unset = open-ended) |
| `pinned` | always injected first; protected from supersede-retirement |
| `retired_at` / `retired_by` | soft retirement: kept for audit, never injected; `retired_by` links the superseding memory |

**Two time axes, deliberately distinct:** `valid_from/valid_to` describe the
world; `learned_at`/`retired_at` describe fleet's bookkeeping. Retiring a fact
does not claim it stopped being true — it stops fleet from citing it.

## Lifecycle

- **Write is approval-gated** (unchanged): the agent's `propose_memory` tool
  and the background extractor only create *proposals*; a human clicks
  Save/Don't-save. Manual creation in the memory manager is immediate.
- **Injection**: active (non-retired, non-proposed) memories, pinned first
  then newest, capped at 50. Non-`fact` kinds and validity windows are
  annotated on the bullet — explainability and a recency tiebreaker;
  **retirement is the mechanism that stops stale citations**.
- **Edit / pin / retire / restore / delete**: `PATCH /memories/{id}` takes a
  partial body `{content?, kind?, pinned?, retired?, valid_from?, valid_to?}`
  (0 clears a validity bound); `DELETE` forgets outright. The memory manager
  modal (brain icon) exposes all of it, including a retired section with
  Restore.

## Contradiction candidates (stage 2)

The post-turn extractor can flag that a new fact *supersedes* an existing
memory. That claim is surfaced on the proposal card ("replaces: …") and takes
effect only on human Accept — the superseded memory is then retired with
`retired_by` linking its replacement, guarded: not if pinned, not if it was
edited since the claim was made (content-hash check), not if already retired.
Nothing is ever retired silently.

## Honest scope (deferred — the issue's "Later" stage)

- No entity/relation graph tables and no as-of-date queries yet — `kind` +
  the two time axes are the graph-ready foundation, not the graph.
- User-scoped only: team/project memory needs the Projects concept (#509).
- No automatic (non-human-confirmed) retirement policy.
- PII classification belongs to the rampart integration (#450).
