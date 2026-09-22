# Model defaults: GPT-6 Luna Pro everyday, Claude Opus 5.5 as the strong tier

**Status:** shipped 2026-09-22 (previous swap 2026-09-21, recorded below). Design
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
6. **User guide**: update the two names in `internal/clientconfig/builtin_skills/fleet-guide/chat.md`,
   then `make sync-guides`. The drift test checks the guide names both labels.
7. **Docs**: a row in the table below, plus the pools in
   [UPSTREAM-ROUTING-FLOOR.md](UPSTREAM-ROUTING-FLOOR.md).

Deployments that set `FLEET_DEFAULT_MODEL` / `FLEET_ADVANCED_MODEL`, or the admin model
tiers, are unaffected by any of this. Only the compiled-in fallback moves.

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
