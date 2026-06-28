-- 016_title_locked.sql — protect a user's manual conversation rename from being
-- clobbered by a background auto-title (#302).
--
-- title_locked = FALSE (default): auto-titling may overwrite the title.
-- title_locked = TRUE: the user renamed it manually; auto-titling skips it.
ALTER TABLE conversations ADD COLUMN title_locked BOOLEAN NOT NULL DEFAULT FALSE;
