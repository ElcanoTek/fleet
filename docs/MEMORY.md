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

## Knowledge graph (stage 3, #523)

The temporal knowledge graph is DERIVED, PROVENANCE-LINKED data over the
memories table (chat migration 030; ADR-0029). Memories stay the single
source of truth: `memory_entities` holds typed nodes (closed set:
person|organization|place|project|tool|topic|other, unknown→other) and
`memory_relations` holds (subject, predicate, object) edges where each edge
links to the memory it was extracted from and the object is either another
entity or a literal value (exactly one, DB-enforced).

**The join-through-memories temporal rule:** relation rows carry NO time
columns. Every as-of answer derives from the source memory's own axes through
the join — `learned_at`/`retired_at` (transaction time) and
`valid_from`/`valid_to` (valid time) — so the graph can never disagree with
the records it derives from. Retirement cascade is therefore just parent-join
filtering: retiring a memory removes its edges from current views (and
historical views dated before the retirement still show them); deleting a
memory hard-deletes its edges (FK `ON DELETE CASCADE`).

**As-of queries** (store `GraphAsOf`/`ListMemoriesAsOf`; HTTP
`GET /memories/graph` and `GET /memories` with `as_of_valid=`/`as_of_learned=`
RFC3339 params):

- `as_of_learned` — "what did fleet know/trust then": `learned_at <= T` and
  not-yet-retired at T. Unset = now (this axis always applies).
- `as_of_valid` — "what was true in the world then": the validity window
  contains T (open bounds pass). Unset = no filter (windows are optional
  annotations).
- Proposals (`source='proposed'`) are always excluded — an unreviewed
  candidate is not knowledge. `project_id` selects a project's shared scope
  (membership-gated); default is personal.

**Extraction is gated and best-effort.** `FLEET_MEMORY_GRAPH_ENABLED`
(default FALSE): when off, behavior is byte-for-byte unchanged — no model
call, no rows. When on, a memory becoming ACTIVE (manual create, accepted
proposal) fires a detached, time-bounded extraction against the cheap
`FLEET_MEMORY_GRAPH_MODEL` (defaults through MEMORY→METADATA→TITLE model),
schema-validated; any failure is a log line and zero rows, never a
user-visible error. `POST /memories/{id}/extract-graph` re-runs extraction
for one memory on demand (same flag; re-extraction replaces that memory's
edges atomically — idempotent). The memory manager's Graph tab renders the
result with both as-of inputs.

## Honest scope (deferred — per the issue's own triage)

- **Deterministic auto-conflict rules are STILL deferred** (#523 defers them
  explicitly): the graph adds no automatic retirement or contradiction
  resolution. Conflict handling remains the stage-2 human-confirmed
  supersession, until pin/retire/supersede acceptance rates in the field show
  what the right policy is.
- Graph extraction covers personal memories (manual create + accepted
  proposals). Project-scoped memories are queryable via `project_id` but are
  not auto-extracted, and team-wide scoping beyond project memories is
  deferred.
- Extraction is default-off and LLM-derived: the graph is a lossy projection
  of the records, not a second source of truth.
- No automatic (non-human-confirmed) retirement policy.
- PII classification belongs to the rampart integration (#450).
