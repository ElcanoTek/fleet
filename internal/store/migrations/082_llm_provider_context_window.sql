-- 082_llm_provider_context_window.sql — the context window of an admin-managed
-- LLM provider, the admin-row twin of a bundle entry's context_window_tokens.
--
-- Native and OpenAI-compatible endpoints advertise no reliable context limit,
-- so a provider with none declared runs on a conservative fallback (32,000
-- tokens, or 4,096 for Ollama). With the default tool set that fallback sits
-- below what a turn reserves before any history (system prompt, tool schemas,
-- the completion allowance and a provider margin), so an admin row, which could
-- not declare a limit until now, failed its turns before a request was sent.
-- Rows store the operator's declared limit here; the API requires it before a
-- non-OpenRouter row can be enabled. OpenRouter rows ignore it whatever it
-- holds: OpenRouter's per-model catalog is authoritative.
--
-- 0 = not declared, which is every row written before this column. Those keep
-- the fallback exactly as before, so a defaulted ADD is the old behaviour.
ALTER TABLE llm_providers ADD COLUMN context_window_tokens INTEGER NOT NULL DEFAULT 0
    CHECK (context_window_tokens >= 0);
