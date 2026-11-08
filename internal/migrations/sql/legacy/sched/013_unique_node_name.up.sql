-- Enforce unique node names.
--
-- RegisterNode was previously check-then-act (GetNodeByName then AddNode) with
-- no DB constraint, so two concurrent registrations of the same name could
-- create duplicate rows, after which name-targeted tasks matched both. This
-- migration collapses any pre-existing duplicates, then adds a UNIQUE index so
-- the application can rely on INSERT ... ON CONFLICT for atomic re-registration.
--
-- Note: tasks.assigned_node_id / target_node_id are plain UUID columns (no FK),
-- so re-pointing them at the surviving row keeps in-flight and pending work
-- attached to a real node instead of orphaning it.

-- Re-point in-flight assignments from duplicates to the survivor (the most
-- recently active row per name).
WITH ranked AS (
    SELECT id, name,
           first_value(id) OVER (
               PARTITION BY name
               ORDER BY last_heartbeat DESC, registered_at DESC
           ) AS keep_id
    FROM nodes
)
UPDATE tasks t
SET assigned_node_id = r.keep_id
FROM ranked r
WHERE t.assigned_node_id = r.id AND r.id <> r.keep_id;

WITH ranked AS (
    SELECT id, name,
           first_value(id) OVER (
               PARTITION BY name
               ORDER BY last_heartbeat DESC, registered_at DESC
           ) AS keep_id
    FROM nodes
)
UPDATE tasks t
SET target_node_id = r.keep_id
FROM ranked r
WHERE t.target_node_id = r.id AND r.id <> r.keep_id;

-- Remove the duplicate node rows, keeping the most recently active per name.
WITH ranked AS (
    SELECT id,
           row_number() OVER (
               PARTITION BY name
               ORDER BY last_heartbeat DESC, registered_at DESC
           ) AS rn
    FROM nodes
)
DELETE FROM nodes WHERE id IN (SELECT id FROM ranked WHERE rn > 1);

-- The unique index supersedes the plain lookup index on name.
DROP INDEX IF EXISTS idx_nodes_name;
CREATE UNIQUE INDEX IF NOT EXISTS idx_nodes_name_unique ON nodes(name);
