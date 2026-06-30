-- 025_thinking_config.sql — per-conversation Claude extended-thinking override (#220).
--
-- thinking_config : nullable JSONB holding {"enabled": bool, "budget_tokens": int}.
--                   NULL (the default for every existing and new row) means
--                   "inherit the global FLEET_DEFAULT_THINKING_BUDGET_TOKENS
--                   default"; a non-NULL value is an explicit per-chat choice
--                   (enabled=false force-disables thinking even when a global
--                   default is set). Set via PUT /conversations/{id}/thinking_config.
--
-- Nullable with no default, so existing conversations are untouched — thinking
-- stays off unless an operator sets the global default or a user opts a specific
-- conversation in. The producer (internal/agentcore) clamps budget_tokens into
-- Claude's accepted [1024, 100000] window and silently ignores it on non-Claude
-- models, so a stored value can never produce an invalid request.
ALTER TABLE conversations
    ADD COLUMN thinking_config JSONB;
