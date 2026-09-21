# Model defaults: GPT-5.6 Luna Pro everyday, Claude Opus 5 as the strong tier

**Status:** shipped 2026-09-21. Design note for the default-model swap.

## What changed

| Slot | Before | After |
|---|---|---|
| `DefaultCoreModel` (new chat conversations, scheduled tasks without a pinned model, the Operations Center create form) | `google/gemini-3.8-flash` | `openai/gpt-5.6-luna-pro` |
| `DefaultMaxModel` / chat's "advanced model" (`suggest_advanced_model` escalation) | `openai/gpt-5.6-sol` | `anthropic/claude-opus-5` |
| `DefaultTitleModel` (conversation titles) | `google/gemini-3.8-flash` | `openai/gpt-5.6-luna-pro` |
| Web `DEFAULT_MODEL` / `ADVANCED_MODEL` and the task-create form's primary/fallback placeholders | gemini-3.8-flash / gpt-5.6-sol | gpt-5.6-luna-pro / deepseek-v4.1-flash |
| Lockdown allow-list default (one slug per tier) | gemini-3.8-flash, gpt-5.6-sol | gpt-5.6-luna-pro, claude-opus-5 |

Operators override all of these per deployment with `FLEET_DEFAULT_MODEL`,
`FLEET_ADVANCED_MODEL`, `FLEET_TASK_MODEL`, `FLEET_TASK_FALLBACK_MODEL` and
`FLEET_TITLE_MODEL` (or the Settings → Admin → Features model tiers, which
apply live). Production and the client boxes were switched through those knobs
on 2026-09-21 ahead of this change; this PR makes the shipped defaults match.

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
  already soft-pinned to their vendors. The table gains an `officialPool` flag
  so the "default must be strictly pinned or floored" guard can state the third
  safe shape explicitly — a pool made only of official weights — instead of
  being loosened. See [UPSTREAM-ROUTING-FLOOR.md](UPSTREAM-ROUTING-FLOOR.md).
- The scheduled-task fallback is a deployment knob, not a code default;
  production uses `deepseek/deepseek-v4.1-flash` (soft-pinned to DeepSeek with
  the fp8 floor).
- Context-window table: `anthropic/claude-opus-5` and `-sonnet-5` get 1M rows
  ahead of the generic 200K Claude row; `openai/gpt-5.6*` already had 1.05M.

## Deferred

- Retiring `google/gemini-3.8-flash` and `openai/gpt-5.6-sol` from the fake-LLM
  catalog and test fixtures: they stay selectable and several tests use them as
  fixture slugs, so they remain listed.
