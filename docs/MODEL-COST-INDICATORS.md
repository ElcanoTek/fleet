# Model cost indicators ($ … $$$$)

Design note for the restaurant-style price tier shown next to each model in
fleet's two model pickers. Honest scope: this is a **UI comparison aid** over
the OpenRouter catalog's published prices. It does not change routing, spend
governance, or what a run actually costs.

## What shipped

Both pickers render a four-glyph price indicator per model — one to four filled
`$` against a dimmed remainder, so `$$` reads as *two of four* rather than as an
unqualified "cheap-ish":

- **Chat composer** (`web/src/app/chat/ui/Composer.tsx`) — on every row of the
  model listbox, in a meta cluster at the row's trailing edge next to the single
  existing status pill (`recommended` / `workspace` / `tested` / `✨ new` /
  `experimental`), **and** on the collapsed model chip itself, so the price band
  of the model you are about to spend money on is visible without opening the
  picker.
- **Operations center task form** (`web/src/app/shared/ui/ModelPicker.tsx`, used
  by `orchestrator/TaskCreateModal.tsx` for both the primary and the fallback
  model) — in each option's header row after the Workspace/Recommended badge.

Hovering (or reading with a screen reader) gives the full sentence, e.g.
`premium cost — about $6.00/M tokens blended ($$$ of $$$$)`.

## How the tier is computed

`web/src/app/shared/lib/modelCost.ts` is the single pure definition, shared by
both pickers and unit-tested in `modelCost.test.ts`.

OpenRouter prices a model as two per-token numbers. Those are unreadable at a
glance, so we collapse them into one **blended price per million tokens**,
weighted **3 prompt : 1 completion**:

```
blended$/M = ((prompt × 3 + completion × 1) / 4) × 1e6
```

That is the conventional ratio for a single headline price, and it is the right
shape for fleet's agent loops specifically: each step re-sends a growing
transcript and emits comparatively few output tokens, so prompt price dominates
real spend.

Tier thresholds (inclusive upper bounds, blended USD per 1M tokens):

| Tier   | Blended price   | Label      | Typical occupants                      |
| ------ | --------------- | ---------- | -------------------------------------- |
| `$`    | ≤ $1            | budget     | small/fast models, free tiers          |
| `$$`   | ≤ $5            | moderate   | mid-tier workhorses                    |
| `$$$`  | ≤ $15           | premium    | frontier chat models                   |
| `$$$$` | > $15           | top tier   | reasoning flagships                    |

Colour follows the tier (`--color-success` → `--color-warning` →
`--color-danger`), defined once as `.model-cost*` in `globals.css` so the same
component drops into the Tailwind-authored composer and the CSS-class-authored
task form without a variant flag.

## What it deliberately does **not** do

- **No indicator when pricing is unknown.** Workspace-provider models
  (Settings → Admin → Model providers) carry no catalog pricing, and a
  half-typed custom slug isn't in the catalog. Those render *nothing* — an
  absent indicator is honest, a guessed one is not. `null`/`undefined`/`""`
  prices are rejected explicitly rather than coerced (`Number(null) === 0`
  would have shown an unpriced model as free).
- **Not a quote and not a limit.** Real spend depends on the turn's token
  counts, cache hits, and tool loop length. The per-run cost ceilings
  (`FLEET_MAX_COST_USD`, per-task overrides) remain the only thing that
  *governs* spend; there is still deliberately no price ceiling on model
  selection (see `web/src/app/lib/openrouterModels.ts`).
- **No new network calls.** The prices already existed in the cached OpenRouter
  catalog. Two existing same-origin proxies just widened their response:
  `/api/model-catalog` adds `price_prompt` (it already sent
  `price_completion`), and `/api/model-rankings` adds both — the rankings rows
  are what browse mode shows before the larger catalog fetch lands, and the only
  source when the catalog fetch fails. Both changes are additive.

## Tests

- `web/src/app/shared/lib/modelCost.test.ts` — the blend, the thresholds, the
  formatting, and the unknown-pricing cases (including that `null` is *not*
  coerced to a free $0).
- `web/src/app/shared/ui/ModelPicker.test.tsx` — the task-form picker renders
  the tier for a priced catalog model and renders nothing when prices are absent.
- `web/src/app/shared/lib/models.test.ts` — the seed-enrichment join (curated
  name/`recommended` win, prices fill in, no duplicate row).
- `web/e2e/mocked/model-cost.spec.ts` — mocked Playwright over both real
  surfaces: one indicator per row with the expected tier in each picker, and
  selecting a model carries its tier onto the composer chip. The default mocks
  serve empty model lists, so every other spec keeps covering the
  no-pricing/no-indicator path.

## One non-obvious fix that came with it

The two pinned "recommended" slugs are hand-written entries in both pickers, and
dedup keeps the hand-written row while dropping the catalog duplicate. Left
alone, the *recommended* models would have been the only rows in either picker
without a price. So catalog facts (prices, context length, created) are now
joined back onto them — `enrichFromCatalog()` in `shared/lib/models.ts` for the
task form, and a `pricesFor()` lookup in `chat-experience.tsx` for the composer.
Curated fields (name, `recommended`) still win; only missing facts are filled.
