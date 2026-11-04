# Out-of-Credits Notice (`credits_exhausted`)

## What shipped

- **Provider credit/spend-limit classification (`internal/agentcore/provider_stream_error.go`, `internal/agentcore/resilience.go`, `internal/agentcore/resolver.go`)**:
  - Recognizes both OpenRouter credit-exhaustion responses and upstream quota/credit failures:
    - **HTTP `402 Payment Required`** (`status == 402` or `error.code == "402"`), returned when a request's `max_tokens` cost exceeds remaining key/account credits (`"This request requires more credits, or fewer max_tokens..."`, ` "Insufficient credits..."`).
    - **HTTP `403 Forbidden`** with `error.metadata.limit_source` (`"openrouter_key_limit"` / `"openrouter_credit_limit"` / `"key_limit"` / `"credit_limit"` / `"account_credit_limit"`), `error.metadata.scope` (`"key_limit"` / `"budget"`), or `"Key limit exceeded"`, returned when an OpenRouter API key's dollar spend limit is completely exhausted.
    - **Upstream quota/credit errors** (`insufficient_quota`, `insufficient_credits`, `"credit balance is too low"`, `"exceeded your current quota"`), including when delivered via mid-stream SSE error payloads (`parseSSEStreamError`).
  - Classifies these failures as `streamErrorFatal` (`ReasonCreditsExhausted = "credits_exhausted"`) so the resilience loop does not retry in place or promote a same-provider fallback model on the same exhausted key, while preserving cross-provider failover when `fallback_providers` has a candidate on a different configured provider ([ADR-0067](adr/0067-fallback-on-provider-rejection.md)).
- **User-facing message and SSE hygiene (`internal/agent/manager.go`, `internal/httpapi/summarize.go`)**:
  - Emits `CreditsExhaustedMessage`:
    `"You're out of included credits. Top up your API key, or contact licensing@elcanotek.com to add more credits and keep going."`
  - Clears `raw` on `turn.model_required` when `reason == "credits_exhausted"` so gateway workspace key management URLs and internal spend numbers are not sent to the browser.
  - Uses the same `CreditsExhaustedMessage` when `/conversations/{id}/summarize` fails due to exhausted credits.
- **Chat transcript card (`web/src/app/chat/ui/history.ts`, `web/src/app/chat/ui/ChatTranscript.tsx`)**:
  - Adds `"credits_exhausted"` to `ModelSelectionReason` (and maps legacy `status_code === 402` with `reason === "fatal"` to `"credits_exhausted"`).
  - Renders the card title **"Out of credits"** with a clickable **`Contact licensing@elcanotek.com`** (`mailto:licensing@elcanotek.com?subject=Fleet%20Credits`) link and **Retry**, omitting `"or pick a different model"`.

## Deviations and deferred scope

- **Single dual-purpose message in the core distribution instead of per-bundle config**: To work cleanly for both bring-your-own-key operators (who top up their own API key) and included-credit licensees without adding per-client onboarding configuration in `FLEET_CLIENT_CONFIG_DIR`, the core distribution ships a single dual-purpose message instructing the user to either top up their API key or contact Fleet's upstream licensing address (`licensing@elcanotek.com`, matching the maintainer contacts in `README.md` and `SECURITY.md`).
