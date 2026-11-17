-- 0202_sidebar_layouts: saved left-rail ("sidebar") layouts — one per user,
-- plus one workspace default an admin saves. The server stores and validates
-- the layout JSON (internal/sidebar); the client resolves the effective one
-- (user ?? workspace default ?? built-in).

CREATE TABLE IF NOT EXISTS chat.sidebar_layouts (
    user_email TEXT   PRIMARY KEY,
    layout     JSONB  NOT NULL,
    updated_at BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS chat.workspace_sidebar_layout (
    id         SMALLINT PRIMARY KEY CHECK (id = 1),
    layout     JSONB    NOT NULL,
    updated_by TEXT     NOT NULL DEFAULT '',
    updated_at BIGINT   NOT NULL
);
