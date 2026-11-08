# ADR-0111: Remove the derived memory knowledge graph; enable memory auto-indexing by default

- **Status:** Accepted (supersedes ADR-0029; amends ADR-0019)
- **Date:** 2026-11-08
- **Deciders:** fleet maintainers

## Context

ADR-0029 added a derived knowledge-graph projection (`memory_entities` and
`memory_relations`, `GET /memories/graph`, `POST /memories/{id}/extract-graph`,
`GET /memories?as_of_valid=...&as_of_learned=...`, and the web UI's Graph tab)
gated behind `FLEET_MEMORY_GRAPH_ENABLED` (default off). In practice, the flat,
typed, provenanced memory records (ADR-0019) and human-confirmed contradiction
retirement already answer what users and agents need, whereas the derived graph
view added an unused UI tab, an extra LLM extraction seam, and two extra
configuration settings without improving agent retrieval or operator workflows.

Meanwhile, post-turn memory suggestions (`FLEET_MEMORY_AUTOINDEX_ENABLED` /
`memory_autoindex_enabled`) remain human-gated (surfacing reviewable proposals
rather than mutating state silently) and should be on by default on every
installation.

## Decision

1. **Remove the derived memory knowledge graph.** Delete the LLM graph
   extractor (`internal/agent/memory_graph.go`), the store graph/as-of queries
   (`internal/store/memorygraph.go`), the HTTP graph/as-of routes and seams
   (`GET /memories/graph`, `POST /memories/{id}/extract-graph`,
   `as_of_valid`/`as_of_learned` on `GET /memories`, `GET /api/memories/graph`),
   and the web UI's Graph tab (`MemoryGraphView.tsx`, `memoryGraph.ts`).
2. **Retire `FLEET_MEMORY_GRAPH_ENABLED` and `FLEET_MEMORY_GRAPH_MODEL`.** Move
   both settings (and their removed `CHAT_`/`CUTLASS_` spellings) into
   `envreg`'s `removedSettings` so any leftover line in `fleet.env` or the
   process environment reads as unset and is reported by boot, `fleet doctor`,
   `fleet config explain`, and `fleet validate-config --retired-only`.
3. **Enable `FLEET_MEMORY_AUTOINDEX_ENABLED` by default (`true`).** It remains
   an approval-gated proposal loop; operators can still disable
   `memory_autoindex_enabled` live in Settings → Admin → Features (or via
   `FLEET_MEMORY_AUTOINDEX_ENABLED=false`).

## Consequences

- User memories remain the single source of truth (`memories` table, ADR-0019)
  with no parallel graph projection or extra model call on memory save/accept.
- Leftover `FLEET_MEMORY_GRAPH_*` lines on existing boxes are flagged by
  `fleet doctor` with a clear removal message.
- New and existing deployments get post-turn memory suggestions out of the box
  without manual env-file opt-in.
