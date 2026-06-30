-- 023_conversation_branching.sql — fork a conversation at a chosen message (#454).
--
-- parent_conversation_id   : when non-NULL, this conversation is a BRANCH of that
--                            conversation — it was forked from it, copying the
--                            parent's messages up to (and including)
--                            branch_point_message_id, then diverging. NULL = a
--                            normal, non-branch conversation.
-- branch_point_message_id  : the messages.id in the PARENT the fork was taken at.
--                            NULL on non-branch conversations.
--
-- The branch is a fully independent conversation (its messages are copied, not
-- shared), so deleting the parent never orphans it; these columns are lineage
-- metadata only. Existing rows default both to NULL (not a branch), so nothing
-- changes for conversations that are never branched. No FK on
-- parent_conversation_id: the parent may be deleted later and the branch must
-- survive, so the pointer is allowed to dangle (lineage, not a hard reference).
ALTER TABLE conversations
    ADD COLUMN parent_conversation_id  TEXT,
    ADD COLUMN branch_point_message_id BIGINT;

-- Lookup index for "show all branches of conversation X" (only branch rows are
-- indexed; non-branch rows have a NULL parent and are excluded).
CREATE INDEX idx_conv_parent ON conversations (parent_conversation_id) WHERE parent_conversation_id IS NOT NULL;
