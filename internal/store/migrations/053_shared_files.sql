-- 053_shared_files.sql — the native cross-chat shared file library.
--
-- Metadata rows for admin-uploaded files that every conversation can read
-- (docs/SHARED-FILES.md). The BYTES are not here: the canonical copy lives
-- host-side under <DataDir>/shared_files/<id> (control-plane state, never
-- mounted into a sandbox), and a staged read-only copy is materialized under
-- <WorkspaceRoot>/shared/[folder/]name — the one tree that is visible inside
-- sandboxes on BOTH backends (podman bind mount / kubernetes workspace claim).
-- This table is the manifest the stager reconciles that tree against.
--
-- (folder, name) is the staged path, so it must be unique — two rows may not
-- claim the same file inside the library. folder is a single sanitized path
-- segment ('' = library root); name is a sanitized basename. Both are
-- validated at the API layer before any row is written.
CREATE TABLE IF NOT EXISTS shared_files (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    folder       TEXT NOT NULL DEFAULT '',
    description  TEXT NOT NULL DEFAULT '',
    size_bytes   BIGINT NOT NULL,
    content_type TEXT NOT NULL DEFAULT '',
    sha256       TEXT NOT NULL DEFAULT '',
    uploaded_by  TEXT NOT NULL DEFAULT '',
    created_at   BIGINT NOT NULL,
    updated_at   BIGINT NOT NULL,
    UNIQUE (folder, name)
);
