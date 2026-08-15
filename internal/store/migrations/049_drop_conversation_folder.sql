-- 049_drop_conversation_folder.sql — retire conversation folders (#258/#279).
--
-- Folders were the flat, single-assignment bucket a conversation could sit in.
-- Projects (#509, ADR-0021) superseded them — they are the binding object
-- folders never were (standing instructions, curated connectors, shared memory,
-- membership) — and the folders UI was removed when projects shipped. What
-- remained was a write surface with no writer: no client set `folder`, so every
-- row has carried the '' default since, while the server still served
-- GET /folders, POST /folders/rename and a ?folder= filter nothing called.
--
-- Dropping the column (and its now-unreachable index) removes the last of that
-- half-migrated feature. Labels — the multi-assignment sibling added by the same
-- migration — stay: they are live in the rail today.
--
-- Filing a conversation used to auto-pin it, so previously-filed chats are
-- already visible under Pinned; nothing becomes unreachable.

-- migration-lint: allow-dangerous  drops the folder column retired by projects (#509); the folders UI was removed and no client has written it since
ALTER TABLE conversations DROP COLUMN IF EXISTS folder;

-- idx_conv_user_folder (user_email, folder) is dropped with its column by
-- Postgres; DROP INDEX IF EXISTS makes that explicit and keeps a partially
-- hand-patched database converging on the same shape.
DROP INDEX IF EXISTS idx_conv_user_folder;
