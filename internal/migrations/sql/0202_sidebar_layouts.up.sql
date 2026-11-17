-- 0202_sidebar_layouts: saved left-rail ("sidebar") layouts — one per user,
-- plus one workspace default an admin saves. The server stores and validates
-- the layout JSON (internal/sidebar); the client resolves the effective one
-- (user ?? workspace default ?? built-in).

-- rev orders a person's saves: the browser numbers each edit, and a save
-- older than the stored one is ignored, so two requests that overlap (a save
-- still in flight when the page unloads and sends the next) can never leave
-- the older layout on top. A reset stores the JSON value null with its rev
-- rather than deleting the row, for the same reason.
CREATE TABLE IF NOT EXISTS chat.sidebar_layouts (
    user_email TEXT   PRIMARY KEY,
    layout     JSONB  NOT NULL,
    updated_at BIGINT NOT NULL,
    rev        BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS chat.workspace_sidebar_layout (
    id         SMALLINT PRIMARY KEY CHECK (id = 1),
    layout     JSONB    NOT NULL,
    updated_by TEXT     NOT NULL DEFAULT '',
    updated_at BIGINT   NOT NULL
);
