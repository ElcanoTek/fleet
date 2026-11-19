import { formatFixed } from "@/app/lib/i18n";
import { t as tr } from "@/app/lib/i18n";
// Credit-usage levels for the model pickers (a four-step, colour-coded meter).
//
// Money is shown to people as "credits" everywhere in the UI (one credit is one
// US dollar of metered provider spend — see shared/lib/credits.ts); no "$"
// appears. OpenRouter prices every model as two per-token numbers (prompt and
// completion). Those are unreadable at a glance — 0.0000006 vs 0.000002 tells
// a user nothing while they are choosing a model mid-conversation. This module
// collapses the pair into one *blended* price per million tokens and buckets it
// into four levels, drawn as a meter that fills and warms as usage rises.
//
// The blend is weighted 3 prompt : 1 completion — the industry-conventional
// ratio for a single headline price, and the right shape for fleet's agent
// loops, which re-send a growing transcript on every step and emit comparatively
// few output tokens. It is a *comparison aid*, not a quote: real spend depends
// on the turn, and the per-run ceilings (FLEET_MAX_COST_USD) are what actually
// govern it.
//
// Pure module — no React, no fetch — so both pickers (chat composer and the
// orchestrator task form) share one definition and one set of tests.

export type ModelCostTier = 1 | 2 | 3 | 4;

export type ModelPrices = {
  // USD per prompt/completion token, exactly as OpenRouter reports them.
  // Either may be missing: workspace-provider models carry no catalog
  // pricing, and third-party listings occasionally omit one side.
  pricePrompt?: number | null;
  priceCompletion?: number | null;
};

export type ModelCost = {
  tier: ModelCostTier;
  // Blended credits per 1M tokens (3 prompt : 1 completion).
  blendedPerMillion: number;
  // Level name for the accessible label ("Low credit usage", …).
  label: string;
  // Full human sentence used as the title/aria text on the indicator.
  description: string;
};

export const MAX_COST_TIER = 4;

const PROMPT_WEIGHT = 3;
const COMPLETION_WEIGHT = 1;

// Upper bounds (inclusive) in blended USD per 1M tokens. The top tier has no
// bound. Chosen so the tiers separate the families users actually compare:
// small/fast models land in level 1, mid-tier workhorses in 2, frontier
// models in 3, and the reasoning flagships in 4.
const TIER_MAX_PER_MILLION: ReadonlyArray<number> = [1, 5, 15];

const TIER_LABELS: Readonly<Record<ModelCostTier, string>> = {
  1: tr("Low credit usage"),
  2: tr("Moderate credit usage"),
  3: tr("High credit usage"),
  4: tr("Very high credit usage"),
};

// finite accepts only a genuine non-negative number (or a numeric string, the
// shape OpenRouter sometimes uses). null / undefined / "" are rejected rather
// than coerced: Number(null) === 0 would render an *unpriced* model as free,
// which is the one wrong answer worse than showing no indicator at all.
function finite(value: unknown): number | null {
  if (value === null || value === undefined) return null;
  if (typeof value === "string" && value.trim() === "") return null;
  if (typeof value !== "number" && typeof value !== "string") return null;
  const n = Number(value);
  return Number.isFinite(n) && n >= 0 ? n : null;
}

// blendedPricePerMillion returns the 3:1 weighted price per 1M tokens, or null
// when neither side is known. A single known side is used alone rather than
// discarded — half the signal beats no indicator at all.
export function blendedPricePerMillion(prices: ModelPrices): number | null {
  const prompt = finite(prices.pricePrompt);
  const completion = finite(prices.priceCompletion);
  if (prompt === null) return completion === null ? null : completion * 1e6;
  if (completion === null) return prompt * 1e6;
  const blended =
    (prompt * PROMPT_WEIGHT + completion * COMPLETION_WEIGHT) /
    (PROMPT_WEIGHT + COMPLETION_WEIGHT);
  return blended * 1e6;
}

export function tierForBlendedPrice(perMillion: number): ModelCostTier {
  for (let i = 0; i < TIER_MAX_PER_MILLION.length; i++) {
    if (perMillion <= TIER_MAX_PER_MILLION[i]) return (i + 1) as ModelCostTier;
  }
  return 4;
}

// formatBlendedPrice renders the blended price for the tooltip, in credits.
// Prices under one credit get three decimals so 0.075 doesn't round to 0.08;
// everything else gets two.
export function formatBlendedPrice(perMillion: number): string {
  if (perMillion === 0) return "free";
  const decimals = perMillion < 1 ? 3 : 2;
  return tr("{0} credits/M tokens", [formatFixed(perMillion, decimals)]);
}

// modelCostFor is the one entry point the UI calls. Returns null when the model
// has no known pricing (workspace providers, free-typed slugs not in the
// catalog) — callers render nothing rather than guessing a tier.
export function modelCostFor(prices: ModelPrices | null | undefined): ModelCost | null {
  if (!prices) return null;
  const perMillion = blendedPricePerMillion(prices);
  if (perMillion === null) return null;
  const tier = tierForBlendedPrice(perMillion);
  const label = TIER_LABELS[tier];
  const priceText = formatBlendedPrice(perMillion);
  return {
    tier,
    blendedPerMillion: perMillion,
    label,
    description:
      perMillion === 0
        ? tr("Free — no per-token charge")
        : tr("{0} — about {1} blended (level {2} of {3})", [label, priceText, tier, MAX_COST_TIER]),
  };
}
