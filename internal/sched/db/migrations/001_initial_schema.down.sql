-- Rollback initial schema
-- WARNING: This will delete all data!

DROP INDEX IF EXISTS idx_users_session_token;
DROP INDEX IF EXISTS idx_users_username;
DROP INDEX IF EXISTS idx_users_token_expires_at;
DROP INDEX IF EXISTS idx_tasks_fetch_optimization;
DROP INDEX IF EXISTS idx_tasks_assigned_node;
DROP INDEX IF EXISTS idx_tasks_priority;
DROP INDEX IF EXISTS idx_tasks_created_at;
DROP INDEX IF EXISTS idx_tasks_status;
DROP INDEX IF EXISTS idx_tasks_completed_at;
DROP INDEX IF EXISTS idx_nodes_previous_api_key;
DROP INDEX IF EXISTS idx_nodes_name;
DROP INDEX IF EXISTS idx_nodes_api_key;
DROP INDEX IF EXISTS idx_nodes_heartbeat;
DROP INDEX IF EXISTS idx_nodes_status;

DROP TABLE IF EXISTS logs;
DROP TABLE IF EXISTS tasks;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS nodes;
