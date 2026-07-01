-- 028_projects.sql — Projects / Spaces: shared team workspaces (#509).
--
-- A project is the BINDING object folders never were: standing instructions,
-- a curated connector (optional-MCP) selection, default persona/model, a
-- shared memory scope, and membership. Membership reuses the #237 team RBAC
-- model: a project with a team_id is visible/usable by every user sharing
-- that team_id (plus the owner); an empty team_id = personal project.
-- Only the OWNER edits the project definition.
CREATE TABLE IF NOT EXISTS projects (
    id              TEXT PRIMARY KEY,
    owner_email     TEXT NOT NULL,
    name            TEXT NOT NULL,
    instructions    TEXT NOT NULL DEFAULT '',
    team_id         TEXT NOT NULL DEFAULT '',
    default_persona TEXT NOT NULL DEFAULT '',
    default_model   TEXT NOT NULL DEFAULT '',
    -- Curated optional-MCP server names (from the global catalog; credentials
    -- stay host-side exactly as for any conversation-level opt-in).
    mcp_servers     JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at      BIGINT NOT NULL,
    updated_at      BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_projects_owner ON projects (owner_email);
CREATE INDEX IF NOT EXISTS idx_projects_team ON projects (team_id) WHERE team_id != '';

-- A conversation may belong to one project (set at creation; NULL = none).
-- Deleting a project DETACHES its conversations (history is the user's).
ALTER TABLE conversations ADD COLUMN IF NOT EXISTS project_id TEXT;
CREATE INDEX IF NOT EXISTS idx_conversations_project ON conversations (project_id) WHERE project_id IS NOT NULL;

-- Project-scoped shared memory (#509 + #515 scope-awareness): a memory row
-- with project_id set belongs to the PROJECT (visible to members, injected
-- into project conversations), not to the writer's personal memory. Personal
-- memory queries exclude these rows.
ALTER TABLE memories ADD COLUMN IF NOT EXISTS project_id TEXT;
CREATE INDEX IF NOT EXISTS idx_memories_project ON memories (project_id) WHERE project_id IS NOT NULL;
