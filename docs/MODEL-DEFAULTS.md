# Model defaults: Claude Opus 5.5 as primary default, Gemini 4 Argon as fallback frontier tier

**Status:** shipped 2026-10-28 (previous swaps 2026-10-15, 2026-10-14, 2026-09-22 and 2026-09-21, recorded below). Design
note for the default-model swaps, and the checklist for the next one.

## Changing the defaults (checklist)

The compiled-in tiers are meant to change often. There are now two places that
hold the slugs, plus data that has to be checked by hand:

1. **`internal/agentcore/models.go`**: `DefaultCoreModel` / `DefaultMaxModel`, and a
   one-paragraph comment on each with price, window and endpoints. `config.DefaultTitleModel`
   and the fake-LLM catalog derive from these constants.
2. **`web/src/app/lib/modelAliases.ts`**: `DEFAULT_MODEL` / `ADVANCED_MODEL` and their
   `*_LABEL`s (OpenRouter's display `name`). Web code and tests reference these
   constants. `scripts/check_model_defaults_test.go` fails if they disagree with Go.
3. **Look up the data** (no key needed):
   ```sh
   curl -s https://openrouter.ai/api/v1/models | jq '.data[] | select(.id=="<slug>") | {id,name,context_length,pricing}'
   curl -s https://openrouter.ai/api/v1/models/<slug>/endpoints | jq -r '.data.endpoints[] | "\(.provider_name)\t\(.tag)\t\(.quantization)"'
   ```
4. **`internal/agentcore/provider.go` `officialPoolSlugs`**: add each new tier slug with
   the endpoint list and the date. `TestDefaultCoreModelCannotBeServedAtArbitraryPrecision`
   fails until you do. Only add a slug whose whole pool is the vendor plus official
   resellers.
5. **Context window**: if no `modelContextWindows` prefix covers the new slug, add one.
   `TestDefaultTiersResolveTheirContextWindows` checks the static table on its own.
6. **User guide**: update the two names in `internal/clientconfig/builtin_skills/fleet-guide/chat.md`
   and `operations-center.md`, then `make sync-guides`. The drift test
   checks both guides.
7. **Docs**: a row in the table below, plus the pools in
   [UPSTREAM-ROUTING-FLOOR.md](UPSTREAM-ROUTING-FLOOR.md).

Deployments that set `FLEET_DEFAULT_MODEL` / `FLEET_ADVANCED_MODEL`, or the admin model
tiers, are unaffected by any of this. Only the compiled-in fallback moves.

## 2026-10-28 (update): Claude Opus 5.5 as primary default tier, Gemini 4 Argon as fallback frontier tier

| Slot | Before | After |
|---|---|---|
| `DefaultCoreModel` (chat default, pre-filled `task_model` tier, and auxiliary calls) | `anthropic/claude-haiku-5.5` | `anthropic/claude-opus-5.5` |
| `DefaultMaxModel` (fallback frontier model for in-chat patrols, Operations Center task form pre-fill, and `suggest_advanced_model`) | `anthropic/claude-opus-5.5` | `google/gemini-4-argon` |

- **Why: frontier intelligence by default across both Chat and scheduled tasks, with cross-provider fallback.** Interactive chat turns, in-chat patrols, and scheduled tasks all start on `anthropic/claude-opus-5.5`, while `google/gemini-4-argon` serves as the automatic fallback frontier model for in-chat patrol turns, the pre-filled fallback model in the Operations Center task form, and the target of `suggest_advanced_model`.

## 2026-10-28 (earlier): Claude Opus 5.5 restored as the strong tier

| Slot | Before | After |
|---|---|---|
| `DefaultCoreModel` | `anthropic/claude-haiku-5.5` | unchanged |
| `DefaultMaxModel` (`suggest_advanced_model`, chat's "advanced model", and the default `task_model` tier) | `google/gemini-4-argon` | `anthropic/claude-opus-5.5` |

- **Why: reliability on long, tool-heavy turns.** Under the strict `google/` pin (`Only=[Google]`, no upstream fallbacks), `google/gemini-4-argon` suffered repeated mid-stream `504 Upstream idle timeout exceeded` failures when generating large structured/HTML outputs (such as Pages dashboard builds) after 20+ tool steps. `anthropic/claude-opus-5.5` is served by eleven official endpoints across five clouds (Anthropic, Claude Platform on AWS, Amazon Bedrock, Azure, Google Vertex), supports explicit prompt-cache breakpoints, and is already listed in `officialPoolSlugs` and covered by the `anthropic/claude-opus-5` 1,000,000-token context window prefix.
- Gemini 4 Argon remains selectable in the catalog with its exact-slug context window and 32K completion-token floor intact.

## 2026-10-15 (follow-up): Operations Center tasks default to the advanced tier

The Haiku swap below also moved the Operations Center's pre-filled task model to
Haiku, because the form pre-filled the chat default. A third live tier, the
**task model** (`task_model` admin setting, env `FLEET_TASK_MODEL`), now owns
that slot and follows the advanced tier unless set. Chat keeps the fast default;
tasks get the strong model. See [TASK-MODEL-TIER.md](TASK-MODEL-TIER.md).

## 2026-10-15: Claude Haiku 5.5 as the everyday default

| Slot | Before | After |
|---|---|---|
| `DefaultCoreModel` (new chats, the Operations Center's pre-filled task model, aux calls derived from it) | `openai/gpt-6-luna-pro` | `anthropic/claude-haiku-5.5` |
| `DefaultMaxModel` | `google/gemini-4-argon` | unchanged |

- **Why: speed.** GPT-6 Luna Pro is a reasoning ("pro") model that thinks
  before every visible token. A live benchmark from a production box on
  2026-10-14 measured it at ~5.7s per call on a short prompt and ~10.3s on a
  ~25K-token one (GPT-5.6 Sol, Claude Opus 5.5 and Gemini 4 Argon were 4.0–4.5s
  on the same large prompt), and a data-heavy chat turn makes one such call
  per tool step — customers felt it as "the platform got slower".
- **Claude Haiku 5.5**: $0.10/M in, $0.50/M out — the same price as Luna Pro —
  a 1,000,000-token window and 128K output. Endpoints read 2026-10-15: Anthropic,
  Claude Platform on AWS, Amazon Bedrock ×3, Azure ×2, Google Vertex ×3,
  quantization "unknown" on all; recorded in `officialPoolSlugs`.
- **Routing**: the existing `anthropic/` soft pin (Order=[Anthropic], fallbacks
  to the official resellers) now carries the hot path, and keeps the prompt
  cache on one upstream. The live prompt-cache check already covers Haiku 5.5
  with explicit breakpoints (~11.6K of ~11.9K read, docs/PROMPT-CACHE-CONTRACT.md).
- **Context window**: a `anthropic/claude-haiku-5` → 1,000,000 row, ahead of
  the generic 200K Claude line (which keeps Haiku 4.5 at 200K).
- **Not changed**: fleet's explicit extended-thinking budget
  (`supportsExtendedThinking`) still covers Opus/Sonnet/Fable only, so the
  default does not get a forced thinking budget — the point of the switch is
  answering without a long think. Deployments that set `FLEET_DEFAULT_MODEL` /
  `FLEET_TASK_MODEL` or the admin model tiers keep what they set, and existing
  tasks keep the model they were saved with (`fleet sched task set-model`
  moves them).

## 2026-10-14: Gemini 4 Argon as the strong tier

| Slot | Before | After |
|---|---|---|
| `DefaultCoreModel` | `openai/gpt-6-luna-pro` | unchanged |
| `DefaultMaxModel` (`suggest_advanced_model`, chat's "advanced model", the second lockdown allow-list default) | `anthropic/claude-opus-5.5` | `google/gemini-4-argon` |

- **Gemini 4 Argon**: released on OpenRouter 2026-10-13. $2/M in, $10/M out,
  $0.10/M cache reads (Opus 5.5: $4/$20), 1,048,576-token window. Endpoints read
  2026-10-14: Google (Vertex `global`, `global/priority`) and Google AI Studio
  (default, `flex`, `priority`), quantization "unknown" on all.
- **Routing**: the existing `google/` pin is strict (`Only=[Google]`, no
  fallbacks), so every escalated request goes to Vertex and the prompt cache stays
  on one upstream. That satisfies `TestDefaultCoreModelCannotBeServedAtArbitraryPrecision`
  without an `officialPoolSlugs` entry. AI Studio is not reachable under the pin,
  so the strong tier has one provider behind it, where Opus 5.5 had eleven
  endpoints across five providers.
- **Still a different family from the default**, so an escalation is also a
  provider change.
- **Context window**: an exact-slug row `google/gemini-4-argon` → 1,048,576 was
  added (no family prefix, for the same Nano Banana reason as the 3.x rows).
- **Extended thinking**: fleet's explicit thinking budget
  (`supportsExtendedThinking`) applies to Anthropic Claude slugs only, so it does
  not apply to the strong tier now; Argon uses its own default reasoning. Google
  thought signatures are carried by the existing reasoning-metadata path.
- **Output floor**: Gemini 4 counts thinking against `max_output_tokens`, and
  Google's Argon migration guide says to raise any limit under 32K to at least
  32,768, or the model can spend the whole cap thinking and return an empty or
  truncated answer. fleet's per-call cap defaults to 16,384, so
  `completionTokensForModel` raises the cap to 32,768 for `google/gemini-4*`
  slugs. It is applied to the stream call, the terminal structured-output call,
  and the matching context reserves, so the reserve still equals what is sent.
  It only raises a smaller cap (`FLEET_LLM_MAX_TOKENS` included), never lowers
  one, and the run's cost and token ceilings still bound spend. Fleet does not
  implement Google's long-decode continuation; answers needing more than the cap
  still end at the cap.
- **Explicit caching**: Google lists explicit context caching as unsupported on
  Argon (implicit caching is on). Measured live on 2026-10-14: Argon ignores
  the `cache_control` breakpoints fleet sends (no error, no cache write), and
  its implicit cache covers ~6.7K of an ~8.9K prompt, across user turns too
  once the `runtime_today` note moved into persisted history; see
  [PROMPT-CACHE-CONTRACT.md](PROMPT-CACHE-CONTRACT.md#results-2026-10-14).
- **Not measured**: not benchmarked on production jobs before shipping. Watch
  escalated turns' quality and cost on the first day.
- Opus 5.5 keeps its `officialPoolSlugs` entry and context-window prefix: it stays
  selectable and an admin tier override may still point at it.
- **Guide drift**: the Operations Center guide still named GPT-5.6 Luna Pro as
  the task form's default (stale since 2026-09-22). It is corrected, and
  `TestModelDefaultsAreInSync` now checks that guide names the default too.

## 2026-09-22: GPT-6 Luna Pro + Claude Opus 5.5

| Slot | Before | After |
|---|---|---|
| `DefaultCoreModel` (+ `DefaultTitleModel` and every auxiliary fallback that ends in it) | `openai/gpt-5.6-luna-pro` | `openai/gpt-6-luna-pro` |
| `DefaultMaxModel` (`suggest_advanced_model`, the strong tier) | `anthropic/claude-opus-5` | `anthropic/claude-opus-5.5` |

- **GPT-6 Luna Pro**: $0.10/M in, $0.50/M out, $0.01/M cache reads (5.6 Luna Pro was
  $0.20/$1.20). 1,050,000-token window. Three endpoints, all OpenAI. That's one fewer
  provider than 5.6 Luna Pro had (Azure), so the everyday tier has no second cloud for now.
  The GPT-6 family had no context-window row and would have cold-booted at the 200K
  default, so `openai/gpt-6` → 1,050,000 was added.
- **Claude Opus 5.5**: $4/M in, $20/M out (Opus 5: $5/$25). 1,000,000-token window, and
  the same 11-endpoint official pool as Opus 5. It's covered by the existing
  `anthropic/claude-opus-5` window prefix and by extended-thinking detection.
- **Not measured**: unlike the 2026-09-21 swap, this one wasn't benchmarked on
  production jobs before shipping. It's a same-vendor generation step on both tiers.
  Watch cost and dead-letters on the first day of scheduled runs.
- **Fewer places to change**: `config.DefaultTitleModel` and the fake-LLM catalog now
  derive from the agentcore constants, web tests and the task form use the shared
  constants, and `TestModelDefaultsAreInSync` pins the one remaining copy (web) and the
  guide. See the checklist above.

## 2026-09-21: GPT-5.6 Luna Pro + Claude Opus 5

### What changed

| Slot | Before | After |
|---|---|---|
| `DefaultCoreModel` (new chat conversations, the Operations Center create form's pre-filled primary, the chat "recommended" tier) | `google/gemini-3.8-flash` | `openai/gpt-5.6-luna-pro` |
| `DefaultMaxModel` / chat's "advanced model" (`suggest_advanced_model` escalation) | `openai/gpt-5.6-sol` | `anthropic/claude-opus-5` |
| `DefaultTitleModel` — conversation titles, **and** the end of every auxiliary model's fallback chain in `config.Load`: with their env vars unset, `MetadataModel` (branch names / metadata tools), `ErrorAnalysisModel`, `MemoryModel` (memory extraction), `RecurringTaskModel` (chat → recurring task promotion), `LibraryPromptModel` (save to prompt library) and `MemoryGraphModel` all resolve to this constant | `google/gemini-3.8-flash` | `openai/gpt-5.6-luna-pro` (so every one of those auxiliary jobs moves to Luna Pro unless its own `FLEET_*_MODEL` / `FLEET_METADATA_MODEL` / `FLEET_TITLE_MODEL` is set) |
| Web `DEFAULT_MODEL` / `ADVANCED_MODEL` (compiled-in fallbacks) | gemini-3.8-flash / gpt-5.6-sol | gpt-5.6-luna-pro / claude-opus-5 |
| Task-create form pre-fill (persisted as the task's pinned `model` / `fallback_model`) | hard-coded gemini-3.8-flash / gpt-5.6-sol, blind to admin settings | **live**: primary = the workspace default tier (`/client-config` `default_model`, i.e. the admin's Settings → Model tiers override or `FLEET_DEFAULT_MODEL`), fallback = `FLEET_TASK_FALLBACK_MODEL` when set (`/client-config` `task_fallback_model`), else deepseek-v4.1-flash |
| Lockdown allow-list default (`FLEET_LOCKDOWN_ALLOWED_MODELS` unset) | a second compiled-in copy of the tier slugs (gemini-3.8-flash, gpt-5.6-sol) | **the live model tiers** — `config.LockdownModels()` returns the current default and advanced models, so lockdown defaults to gpt-5.6-luna-pro / claude-opus-5 and follows an admin tier override live; no retired slug lingers. A lockdown conversation whose persisted model fell off the list is moved to the lockdown default (the first entry) on its next turn (`httpapi.reconcileLockdownModelCtx`, on the shared turn-launch path, so direct and queue-drained turns alike; Compact runs on the default without persisting) instead of failing with "model not allowed in lockdown mode". An explicit operator list still wins verbatim. |

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

### Why

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

### What did not change

- The routing pins (`canonicalUpstream`): `openai/` and `anthropic/` were
  already soft-pinned to their vendors. `officialPoolSlugs` lists the two exact
  default slugs whose endpoint pools were checked (vendor + cloud resellers of
  the official weights) so the "default must be strictly pinned or floored"
  guard can state the third safe shape explicitly — per slug, never per family
  — instead of being loosened. That list is now also the request's
  `provider.only` allow-list (#1589), so the pool it names is enforced rather
  than snapshotted; it admits every endpoint both slugs have today, so the
  reseller fallback is unchanged. See
  [UPSTREAM-ROUTING-FLOOR.md](UPSTREAM-ROUTING-FLOOR.md).
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

### Deferred

- Retiring `google/gemini-3.8-flash` and `openai/gpt-5.6-sol` from the fake-LLM
  catalog and test fixtures: they stay selectable and several tests use them as
  fixture slugs, so they remain listed.

## Follow-up: a refused lockdown model names its replacement (#1588)

The allow-list guard on `POST /chat`'s per-turn model override spent a release
*ignoring* a disallowed slug and running the turn on the conversation's stored
model. That kept a stale browser working, but it also meant a deliberate API
client that asked for a model this deployment forbids got a turn on a different
one and was never told. The guard now **refuses** such an override — 400, no
`SetModel`, no turn — and the body names the model the caller may use instead:

```json
{ "error": "model not allowed in lockdown mode",
  "code": "lockdown_model_not_allowed",
  "model": "openai/gpt-5.6-luna-pro" }
```

`code` is the field to branch on; the prose is free to change. `model` is the
conversation's own slug when the allow-list still permits it, otherwise the
lockdown default that `reconcileLockdownModelCtx` would migrate the
conversation to on its next launch — never a slug that would be refused again,
so a client that adopts it and retries gets a turn rather than a second 400. An
allow-list made only of globs has no literal slug to name, so the field is
omitted and the caller has to choose. The web (`useTurnStream`) does exactly
what an API client should: adopts the named model into the picker and resends
the submission **once**, so a tab holding a slug the server has moved past
self-corrects without the user reloading the page.

Nothing is remembered server-side to make this work — the correction is derived
from the allow-list on each request — so it holds across a process restart, for
a second tab, and after any number of migrations. The alternative, a
per-conversation memo of the exact pre-migration slug, failed all three.

What is deliberately unchanged: a client echoing the conversation's **own**
stored slug is still "no opinion" (`postChat` maps it to an empty override), so
a conversation whose persisted model was delisted still migrates on the
turn-launch path instead of 400ing at its owner; and Compact
(`POST /conversations/{id}/summarize`) still substitutes an allowed model
rather than refusing, because it is a button in the UI rather than a model
choice.
