# Model defaults: GPT-5.6 Luna Pro everyday, Claude Opus 5 as the strong tier

**Status:** shipped 2026-09-21. Design note for the default-model swap.

## What changed

| Slot | Before | After |
|---|---|---|
| `DefaultCoreModel` (new chat conversations, the Operations Center create form's pre-filled primary, the chat "recommended" tier) | `google/gemini-3.8-flash` | `openai/gpt-5.6-luna-pro` |
| `DefaultMaxModel` / chat's "advanced model" (`suggest_advanced_model` escalation) | `openai/gpt-5.6-sol` | `anthropic/claude-opus-5` |
| `DefaultTitleModel` — conversation titles, **and** the end of every auxiliary model's fallback chain in `config.Load`: with their env vars unset, `MetadataModel` (branch names / metadata tools), `ErrorAnalysisModel`, `MemoryModel` (memory extraction), `RecurringTaskModel` (chat → recurring task promotion), `LibraryPromptModel` (save to prompt library) and `MemoryGraphModel` all resolve to this constant | `google/gemini-3.8-flash` | `openai/gpt-5.6-luna-pro` (so every one of those auxiliary jobs moves to Luna Pro unless its own `FLEET_*_MODEL` / `FLEET_METADATA_MODEL` / `FLEET_TITLE_MODEL` is set) |
| Web `DEFAULT_MODEL` / `ADVANCED_MODEL` (compiled-in fallbacks) | gemini-3.8-flash / gpt-5.6-sol | gpt-5.6-luna-pro / claude-opus-5 |
| Task-create form pre-fill (persisted as the task's pinned `model` / `fallback_model`) | hard-coded gemini-3.8-flash / gpt-5.6-sol, blind to admin settings | **live**: primary = the workspace default tier (`/client-config` `default_model`, i.e. the admin's Settings → Model tiers override or `FLEET_DEFAULT_MODEL`), fallback = `FLEET_TASK_FALLBACK_MODEL` when set (`/client-config` `task_fallback_model`), else deepseek-v4.1-flash |
| Lockdown allow-list default (`FLEET_LOCKDOWN_ALLOWED_MODELS` unset) | a second compiled-in copy of the tier slugs (gemini-3.8-flash, gpt-5.6-sol) | **the live model tiers** — `config.LockdownModels()` returns the current default and advanced models, so lockdown defaults to gpt-5.6-luna-pro / claude-opus-5 and follows an admin tier override live; no retired slug lingers. A lockdown conversation whose persisted model fell off the list is moved to the lockdown default (the first entry) on its next turn (`httpapi.reconcileLockdownModel`) instead of failing with "model not allowed in lockdown mode". An explicit operator list still wins verbatim. |

Operators override the chat tiers and the title model per deployment with
`FLEET_DEFAULT_MODEL`, `FLEET_ADVANCED_MODEL` and `FLEET_TITLE_MODEL` (or the
Settings → Admin → Features model tiers, which apply live). **Scheduled tasks
are different**: the scheduler resolves an unpinned task's model from
`FLEET_TASK_MODEL` (snapshotted at boot) and refuses to run one when that is
unset — `DefaultCoreModel` is not consulted there. Tasks created through the
Operations Center form arrive already pinned to the form's pre-filled primary
and fallback, so they never hit that path; tasks created through the API or an
import without a pinned model do. `FLEET_TASK_FALLBACK_MODEL` likewise applies
only to tasks with no pinned `fallback_model`. Production and the client boxes
were switched through those knobs on 2026-09-21 ahead of this change; this PR
makes the shipped defaults match.

## Why

Measured on production's scheduled jobs on 2026-09-21 (fleet 2026.09.21.5):

| Model | Runs | Avg prompt tokens | Avg cache hit | Avg cost | Outcome |
|---|---|---|---|---|---|
| gemini-3.8-flash | 15 | 9.4M | 46% | $4.76 | 7 dead-letters, 1 error, 1 paused |
| gpt-5.6-luna-pro (same jobs, re-run) | 13 | 4–10M | 89–95% | $0.16–0.62 | 12 success, 1 verifier/prompt mismatch |

Gemini's OpenRouter uptime was fine (99.76% median over the day); the cost came
from the prompt cache going cold across provider hops — fleet logged 19 cold
starts for gemini-3.8-flash alone — so every step re-paid for the whole
transcript. Luna Pro is served by one vendor family (OpenAI, with Azure and
Bedrock reselling the same weights), which keeps the cache warm, and its cache
reads cost a tenth of Gemini's.

Opus 5 replaces Sol as the strong tier: a different provider family from the
default (so an escalation is also a provider change), a 1M window, eleven
healthy OpenRouter endpoints, and current-generation. Sol's main OpenAI
endpoint was at 80% uptime on the day of the switch and it costs $2/M in.

## What did not change

- The routing pins (`canonicalUpstream`): `openai/` and `anthropic/` were
  already soft-pinned to their vendors. `officialPoolSlugs` lists the two exact
  default slugs whose endpoint pools were checked (vendor + cloud resellers of
  the official weights) so the "default must be strictly pinned or floored"
  guard can state the third safe shape explicitly — per slug, never per family
  — instead of being loosened. See [UPSTREAM-ROUTING-FLOOR.md](UPSTREAM-ROUTING-FLOOR.md).
- There is no compiled-in scheduled-task fallback: form-created tasks carry the
  form's pre-filled `deepseek/deepseek-v4.1-flash` as their own pinned
  fallback, and API/imported tasks without one use `FLEET_TASK_FALLBACK_MODEL`
  (production sets it to the same slug, soft-pinned to DeepSeek with the fp8
  floor).
- The Claude 4 1M-context beta header (`anthropicLongContextSlug`) is not
  extended to Opus 5: every Opus 5 endpoint on OpenRouter lists 1,000,000 as
  its native context and the beta flag is the Claude 4 mechanism. If a >200K
  Opus 5 request is nonetheless refused, the provider rejects it as
  context-too-large and the existing forced-compaction recovery handles it;
  adding the header then is a one-line follow-up with evidence.
- Context-window table: `anthropic/claude-opus-5` and `-sonnet-5` get 1M rows
  ahead of the generic 200K Claude row; `openai/gpt-5.6*` already had 1.05M.

## Deferred

- Retiring `google/gemini-3.8-flash` and `openai/gpt-5.6-sol` from the fake-LLM
  catalog and test fixtures: they stay selectable and several tests use them as
  fixture slugs, so they remain listed.
