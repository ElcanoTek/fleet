-- Recreate the worker-node registry dropped by the up migration (#459).
--
-- This restores the schema as it stood after migrations 001 + 004 + 013 (the
-- unique-name index). Like the other destructive down migrations in this tree
-- the DATA is unrecoverable — only the structure is restored. The application
-- code that read/wrote these objects was removed, so a rollback is schema-only.
CREATE TABLE IF NOT EXISTS nodes (
    id UUID PRIMARY KEY,
    hostname TEXT NOT NULL,
    name TEXT NOT NULL,
    api_key TEXT NOT NULL,
    os_type TEXT NOT NULL,
    status TEXT NOT NULL,
    last_heartbeat TIMESTAMPTZ NOT NULL,
    current_task_id UUID,
    registered_at TIMESTAMPTZ NOT NULL,
    previous_api_key VARCHAR(64),
    key_rotated_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_nodes_status ON nodes(status);
CREATE INDEX IF NOT EXISTS idx_nodes_heartbeat ON nodes(last_heartbeat);
CREATE INDEX IF NOT EXISTS idx_nodes_api_key ON nodes(api_key);
CREATE INDEX IF NOT EXISTS idx_nodes_previous_api_key ON nodes(previous_api_key) WHERE previous_api_key IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_nodes_name_unique ON nodes(name);
CREATE INDEX IF NOT EXISTS idx_nodes_registered_at ON nodes(registered_at);

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS assigned_node_id UUID;
CREATE INDEX IF NOT EXISTS idx_tasks_assigned_node ON tasks(assigned_node_id);
