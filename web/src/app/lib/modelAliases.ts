// Frontend model-slug aliases.
//
// The chat-server has no concept of a "default" model — every /chat
// request carries the slug the UI resolved. So the product's blessed
// slugs live here as named constants. Update either by editing the
// value below; no env plumbing required.
//
// Two tier slots, distinguished by the cost/capability axis we want
// the user to think in:
//   - default  — cheap, fast, the everyday pick
//   - advanced — strongest reasoning + longest context
//
// Beyond the tier slots we also classify every other slug as either
// "tested" (we've validated it works end-to-end with our tools and
// system prompt) or "experimental" (anything else the user types in).
// The picker shows the tier label for the three slots and a small
// tested/experimental tag for everything else, so the user can tell
// at a glance which models we've vouched for.
//
// Keep this list in sync with the server-side lockdown allow-list
// default (server/internal/config/config.go → splitLockdownModels)
// and with the OpenRouter upstream-pin table
// (server/internal/agent/fantasy.go → canonicalUpstream).

// Tier slugs are PINNED to exact model versions — deliberately NOT the
// `~`-prefixed OpenRouter floating aliases (`~google/gemini-flash-latest`
// etc.): fantasy's send-side reasoning reconstruction keys on the slug's
// family prefix, which the `~` sigil defeats, so thinking signatures get
// dropped across tool loops and Anthropic hard-400s with "Invalid
// `signature` in `thinking` block" (root-caused + live-verified
// 2026-06-04; see server/internal/agent/fantasy.go → isAliasModel).
// Trade-off: lab refreshes (Gemini 3.5 → 4 Flash, Opus 4.8 → 4.9) now
// require bumping these constants — and their server-side mirrors
// (agent.AdvancedModelSlug, config.DefaultTitleModel, lockdown
// defaults) — instead of floating automatically.
export const DEFAULT_MODEL = "google/gemini-3.5-flash";
export const DEFAULT_MODEL_LABEL = "default";

export const ADVANCED_MODEL = "anthropic/claude-opus-4.8";
export const ADVANCED_MODEL_LABEL = "advanced";

// TIER_MODELS is the ordered list the picker pins to the top of the
// dropdown when no search query is active.
export const TIER_MODELS: ReadonlyArray<{ slug: string; label: string }> = [
  { slug: DEFAULT_MODEL, label: DEFAULT_MODEL_LABEL },
  { slug: ADVANCED_MODEL, label: ADVANCED_MODEL_LABEL },
];

// TESTED_MODELS lists slugs we've validated end-to-end against our
// tool catalog and system prompt but don't promote to a tier slot.
// Anything not in this set and not a tier slug is treated as
// "experimental" — it should still work, but we haven't checked.
const TESTED_MODELS: ReadonlySet<string> = new Set([
  "openai/gpt-5.4",
]);

export type ModelTier = "default" | "advanced" | "tested" | "experimental";

// labelForModel returns the friendly alias for a tier slug, or the
// raw slug otherwise. Used by the model-picker input + dropdown to
// show "default" / "advanced" instead of the OpenRouter slug.
export function labelForModel(slug: string): string {
  if (slug === DEFAULT_MODEL) return DEFAULT_MODEL_LABEL;
  if (slug === ADVANCED_MODEL) return ADVANCED_MODEL_LABEL;
  return slug;
}

// tierForModel classifies a slug into a UI badge category. Tier slugs
// are returned by their own tier name so the picker can tag them with
// the matching "default" / "advanced" pill; everything else is
// "tested" or "experimental".
export function tierForModel(slug: string): ModelTier {
  if (slug === DEFAULT_MODEL) return "default";
  if (slug === ADVANCED_MODEL) return "advanced";
  if (TESTED_MODELS.has(slug)) return "tested";
  return "experimental";
}
