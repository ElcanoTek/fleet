-- Revert to a non-unique lookup index on node name. The deduplication of rows
-- performed by the up migration is not reversible.
DROP INDEX IF EXISTS idx_nodes_name_unique;
CREATE INDEX IF NOT EXISTS idx_nodes_name ON nodes(name);
