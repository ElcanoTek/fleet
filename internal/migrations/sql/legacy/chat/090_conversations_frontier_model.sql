-- Upgrade existing conversations pinned to retired default/everyday OpenRouter
-- model slugs (e.g. Haiku 5.5, GLM 5.2, GPT-6 Luna, Gemini 3.7, Grok 4.6,
-- Sonnet 5.2, Opus 4.8) to the frontier default model
-- (anthropic/claude-opus-5.5), while preserving custom workspace-provider
-- slugs, operator-configured defaults, and catalog models.
UPDATE conversations
   SET model = 'anthropic/claude-opus-5.5'
 WHERE model IN (
   'anthropic/claude-haiku-5.5',
   'anthropic/claude-sonnet-5.2',
   'anthropic/claude-opus-4.8',
   'z-ai/glm-5.2',
   'openai/gpt-6-luna-pro',
   'google/gemini-3.7-flash',
   'google/gemini-3.7-pro',
   'x-ai/grok-4.6'
 );
