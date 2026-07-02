-- 030_memory_graph.sql — temporal knowledge-graph memory (#523, the deferred
-- "Later" stage of #515). 029 is reserved by a parallel branch; the runner
-- orders by version and tolerates gaps, so the number is safe.
--
-- The graph is DERIVED, PROVENANCE-LINKED data over the memories table:
-- memories stay the single source of truth, every relation links to the
-- memory it was extracted from, and ALL temporal semantics derive through
-- that join (learned_at/retired_at = transaction time, valid_from/valid_to =
-- valid time — 026's axes). The relations table deliberately carries NO time
-- columns of its own: a second bi-temporal surface would inevitably drift
-- from the first (ADR-0019's time-axis discipline, extended by ADR-0029).
-- Retiring or deleting a memory therefore retires/deletes its relations for
-- free — retirement via the as-of join filter, deletion via the CASCADE.

-- One node per (user, normalized name, type). name_norm is the lower/trimmed
-- match key (normalized in Go, like memory kinds); name preserves the display
-- casing from the most recent extraction. entity_type is a closed set
-- normalized in Go: person|organization|place|project|tool|topic|other.
CREATE TABLE IF NOT EXISTS memory_entities (
    id          TEXT PRIMARY KEY,
    user_email  TEXT NOT NULL,
    name        TEXT NOT NULL,
    name_norm   TEXT NOT NULL,
    entity_type TEXT NOT NULL DEFAULT 'other',
    created_at  BIGINT NOT NULL,
    UNIQUE (user_email, name_norm, entity_type)
);
CREATE INDEX IF NOT EXISTS idx_memory_entities_user ON memory_entities (user_email);

-- One edge per extracted (subject, predicate, object) triple, provenance-linked
-- to the memory it derives from. Exactly ONE of object_entity_id (an entity
-- edge) / object_value (a literal attribute) is set — enforced by the CHECK.
-- predicate is a short verb phrase, normalized lower/trimmed (≤64 chars) in Go.
CREATE TABLE IF NOT EXISTS memory_relations (
    id                TEXT PRIMARY KEY,
    user_email        TEXT NOT NULL,
    memory_id         TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    subject_entity_id TEXT NOT NULL REFERENCES memory_entities(id) ON DELETE CASCADE,
    predicate         TEXT NOT NULL,
    object_entity_id  TEXT REFERENCES memory_entities(id) ON DELETE CASCADE,
    object_value      TEXT,
    created_at        BIGINT NOT NULL,
    CHECK ((object_entity_id IS NULL) != (object_value IS NULL))
);
-- Backs the idempotent re-extraction path (delete+insert per memory) and the
-- as-of join.
CREATE INDEX IF NOT EXISTS idx_memory_relations_user_memory ON memory_relations (user_email, memory_id);
