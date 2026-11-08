-- Remove the worker-node registry (#459).
--
-- fleet runs ONE synthetic in-process worker; the in-process runner leases work
-- via a synthetic lease_owner UUID and writes status directly to storage. It
-- never touches the nodes table, and tasks.assigned_node_id has been NULL for
-- every row since per-task node routing was dropped (migration 014). There is no
-- foreign key from tasks to nodes, so these drops are unconstrained. The whole
-- "Registered Agents" surface (HTTP endpoints, dashboard counts, node API keys,
-- the registration token) is removed in the same change.
--
-- IF EXISTS keeps this idempotent, matching the repo's migration style. The node
-- indexes drop with the table.
DROP INDEX IF EXISTS idx_tasks_assigned_node;
ALTER TABLE tasks DROP COLUMN IF EXISTS assigned_node_id;
DROP TABLE IF EXISTS nodes;
