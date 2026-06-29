-- 021_conversation_share.sql — opt-in public read-only sharing for conversations (#226).
--
-- share_token      : when non-NULL, GET /shared/{token} returns the conversation
--                    read-only to anyone the owner gives the link to. Revoke by
--                    NULLing it. 256 bits of crypto/rand, base64url-encoded, so it
--                    is brute-force infeasible — token entropy is the
--                    confidentiality guarantee.
-- shared_at        : unix seconds, set when the token is (re)issued. NULL = not shared.
-- share_expires_at : unix seconds, optional; NULL = never expires. Enforced
--                    server-side in the share lookup, not by the client.
--
-- Existing rows default all three to NULL (not shared), so nothing changes for
-- conversations that are never shared.
ALTER TABLE conversations
    ADD COLUMN share_token      TEXT,
    ADD COLUMN shared_at        BIGINT,
    ADD COLUMN share_expires_at BIGINT;

-- Unique only over issued tokens (NULLs are unconstrained, so unshared rows
-- don't collide). Also the lookup index for GET /shared/{token}.
CREATE UNIQUE INDEX idx_conv_share_token ON conversations (share_token) WHERE share_token IS NOT NULL;
