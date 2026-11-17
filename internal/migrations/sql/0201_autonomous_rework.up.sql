-- 0201_autonomous_rework: user-level Autonomous risk acceptance and
-- server-owned 10-second show_ui auto-submit queue.

-- Downgrade any conversation enabled under the legacy narrow Autonomous
-- allowlist back to Auto until its owner accepts the new Autonomous policy.
UPDATE chat.conversations
   SET approval_mode = 'auto'
 WHERE approval_mode = 'autonomous';

CREATE TABLE IF NOT EXISTS chat.autonomous_acceptances (
    user_email     TEXT   PRIMARY KEY,
    policy_version INT    NOT NULL DEFAULT 1,
    accepted_at    BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS chat.ui_autosubmits (
    id                   TEXT   PRIMARY KEY,
    conversation_id      TEXT   NOT NULL REFERENCES chat.conversations(id) ON DELETE CASCADE,
    user_email           TEXT   NOT NULL,
    call_id              TEXT   NOT NULL,
    action_id            TEXT   NOT NULL,
    values_json          TEXT   NOT NULL,
    deadline_ms          BIGINT NOT NULL,
    status               TEXT   NOT NULL DEFAULT 'pending'
                         CONSTRAINT ui_autosubmits_status_check
                         CHECK (status IN ('pending', 'paused', 'fired', 'cancelled')),
    baseline_user_msg_id BIGINT NOT NULL DEFAULT 0,
    baseline_input_seq   BIGINT NOT NULL DEFAULT 0,
    created_at_ms        BIGINT NOT NULL,
    updated_at_ms        BIGINT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_ui_autosubmits_conv_call_pending
    ON chat.ui_autosubmits (conversation_id, call_id)
    WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS idx_ui_autosubmits_conv_call_created
    ON chat.ui_autosubmits (conversation_id, call_id, created_at_ms DESC);

CREATE INDEX IF NOT EXISTS idx_ui_autosubmits_pending_deadline
    ON chat.ui_autosubmits (deadline_ms)
    WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS idx_ui_autosubmits_conversation_status
    ON chat.ui_autosubmits (conversation_id, status);
