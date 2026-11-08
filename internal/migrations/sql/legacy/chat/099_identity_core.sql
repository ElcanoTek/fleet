-- 099_identity_core.sql — one identity table (#2049 M4, ADR-0105,
-- docs/IDENTITY.md).
--
-- The chat users table becomes core.users, the one table both planes read:
-- chat and the Operations Center. It moves to the neutral schema core of the
-- one database (ADR-0098) — ALTER TABLE ... SET SCHEMA keeps its rows,
-- indexes, CHECK constraints and the team_shared_files foreign key — and
-- gains what the Operations Center needs from a person:
--
--   id         the stable UUID a task or dataset records as its creator; the
--              scheduler's own accounts keep theirs when they are merged in
--              (identity.MergeLegacySchedUsers);
--   ops_scope  Operations Center access for a non-admin: none | view | run
--              (an admin has full access whatever this says);
--   last_login the Operations Center's last sign-in stamp.
--
-- The identity side tables move with it, so the Operations Center reads the
-- session epochs (ADR-0041) from core directly rather than through a seam.
--
-- The identity table lives next to both stores in the one database
-- (ADR-0098). A chat store in public (the removed two-database layout, or a
-- DSN naming no schema) is refused before anything changes.
DO $$
BEGIN
    IF current_schema() IS NULL OR current_schema() = 'public' THEN
        RAISE EXCEPTION 'The chat store must run in schema chat of the one database (DATABASE_URL, docs/ONE-DATABASE.md): the identity table moves into schema core next to it.';
    END IF;
END
$$;

CREATE SCHEMA IF NOT EXISTS core;

ALTER TABLE users SET SCHEMA core;
ALTER TABLE external_access_state SET SCHEMA core;
ALTER TABLE external_auth_epochs SET SCHEMA core;
ALTER TABLE external_logout_events SET SCHEMA core;
ALTER TABLE external_team_renames SET SCHEMA core;

-- gen_random_uuid is volatile, so this rewrites the table once: users is a
-- small table and the migration runs before the server serves.
ALTER TABLE core.users ADD COLUMN id UUID NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE core.users ADD CONSTRAINT users_id_key UNIQUE (id);
ALTER TABLE core.users ADD COLUMN ops_scope TEXT NOT NULL DEFAULT 'none'
    CONSTRAINT users_ops_scope_check CHECK (ops_scope IN ('none', 'view', 'run'));
ALTER TABLE core.users ADD COLUMN last_login TIMESTAMPTZ;

-- One row per one-time identity step that is not a migration (it needs both
-- stores migrated): MergeLegacySchedUsers writes 'm4_sched_users' with its
-- report in the same transaction as the merge.
CREATE TABLE core.identity_merge (
    step    TEXT PRIMARY KEY,
    done_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    report  JSONB NOT NULL DEFAULT '{}'::jsonb
);
