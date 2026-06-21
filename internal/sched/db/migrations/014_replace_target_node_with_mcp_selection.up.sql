-- Replace per-task node routing with a per-task MCP + credential-account
-- selection (plan §6.2). On one box, target_node_* routing is meaningless;
-- mcp_selection carries the [{server, account}] list the run binds host-side.
ALTER TABLE tasks DROP COLUMN IF EXISTS target_node_name,
                  DROP COLUMN IF EXISTS target_node_id;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS mcp_selection JSONB NOT NULL DEFAULT '[]'::jsonb;
