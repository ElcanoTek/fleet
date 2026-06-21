-- Initial schema for MOC
-- This migration is idempotent (uses IF NOT EXISTS) to support both new and existing databases

-- Nodes table: registered Victoria Terminal agents
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

-- Users table: system users for dashboard/API access
CREATE TABLE IF NOT EXISTS users (
    id UUID PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL,
    scopes JSONB DEFAULT '[]',
    created_at TIMESTAMPTZ NOT NULL,
    last_login TIMESTAMPTZ,
    session_token TEXT,
    token_expires_at TIMESTAMPTZ
);

-- Tasks table: work units to be executed by nodes
CREATE TABLE IF NOT EXISTS tasks (
    id UUID PRIMARY KEY,
    prompt TEXT NOT NULL,
    target_node_id UUID,
    target_node_name TEXT,
    priority INTEGER NOT NULL,
    instruction_self_improve BOOLEAN NOT NULL DEFAULT FALSE,
    status TEXT NOT NULL,
    assigned_node_id UUID,
    crush_session_id TEXT,
    created_at TIMESTAMPTZ NOT NULL,
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    result TEXT,
    error_message TEXT,
    scheduled_for TIMESTAMPTZ,
    recurrence TEXT,
    created_by UUID,
    files JSONB DEFAULT '[]'
);

-- Logs table: task execution logs
CREATE TABLE IF NOT EXISTS logs (
    task_id UUID PRIMARY KEY,
    session_data JSONB NOT NULL
);

-- Indexes for efficient queries
CREATE INDEX IF NOT EXISTS idx_nodes_status ON nodes(status);
CREATE INDEX IF NOT EXISTS idx_nodes_heartbeat ON nodes(last_heartbeat);
CREATE INDEX IF NOT EXISTS idx_nodes_api_key ON nodes(api_key);
CREATE INDEX IF NOT EXISTS idx_nodes_name ON nodes(name);
CREATE INDEX IF NOT EXISTS idx_nodes_previous_api_key ON nodes(previous_api_key) WHERE previous_api_key IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_tasks_created_at ON tasks(created_at);
CREATE INDEX IF NOT EXISTS idx_tasks_priority ON tasks(priority);
CREATE INDEX IF NOT EXISTS idx_tasks_assigned_node ON tasks(assigned_node_id);
CREATE INDEX IF NOT EXISTS idx_tasks_fetch_optimization ON tasks(status, priority DESC, created_at ASC);
CREATE INDEX IF NOT EXISTS idx_tasks_completed_at ON tasks(completed_at);
CREATE INDEX IF NOT EXISTS idx_users_username ON users(username);
CREATE INDEX IF NOT EXISTS idx_users_session_token ON users(session_token);
CREATE INDEX IF NOT EXISTS idx_users_token_expires_at ON users(token_expires_at);
