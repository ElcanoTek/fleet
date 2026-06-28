-- 015_archived_at.sql — soft-archive support for conversations (#282).
--
-- archived_at NULL     = active   (appears in the default sidebar view)
-- archived_at non-NULL = archived  (hidden from the default GET /conversations
--                                   list; fetched explicitly via ?archived=true)
--
-- A unix-seconds BIGINT, consistent with created_at / updated_at. Archiving is a
-- soft state distinct from deletion: archived conversations stay fully readable
-- and are excluded from the unpinned-cap eviction (a user-intentional "filed
-- away" state is not clutter).
ALTER TABLE conversations ADD COLUMN archived_at BIGINT;  -- nullable; NULL = active

-- Supports the per-user active/archived split in List and the cap-eviction
-- filter in SweepExpired.
CREATE INDEX idx_conv_user_archived ON conversations (user_email, archived_at);
