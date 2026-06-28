-- Git worktree isolation (#180): an optional per-run worktree config on a task.
--
-- worktree_config is NULL for an ordinary task that shares the workspace root
-- (the prior behaviour). A non-null value with "enabled":true gives each run its
-- own git worktree + branch so concurrent tasks targeting the same repository
-- cannot corrupt each other's working tree. Stored as JSONB; the runner decodes
-- it. The worktree is created as a SUBDIRECTORY of the workspace root (see the
-- WorktreeConfig Go doc for why a /tmp worktree would break git's .git linkage
-- inside the sandbox).
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS worktree_config JSONB;
