// Frontend model-slug constants.
//
// The chat-server has no concept of a "default" model — every /chat
// request carries the slug the UI resolved. So the product's blessed
// slugs live here as named constants. Update either by editing the
// value below; no env plumbing required.
//
// There are no user-facing "default"/"advanced" ALIASES anymore — the
// picker pins two rows by their real display names and the UI always
// shows actual model names. What remains is two ROLE slots:
//   - DEFAULT_MODEL — what a new conversation starts on (shown with the
//     "recommended" pill in the picker)
//   - ADVANCED_MODEL — the stronger escalation target: the model the
//     agent's suggest_advanced_model card and the spreadsheet nudge
//     offer to switch to (also pinned in the picker)
//
// Beyond the two pinned slots we classify every other slug as either
// "tested" (we've validated it works end-to-end with our tools and
// system prompt) or "experimental" (anything else the user types in).
//
// Keep the slugs in sync with the server-side lockdown allow-list
// default (server/internal/config/config.go → splitLockdownModels)
// and the agentcore mirrors (agentcore/models.go → DefaultCoreModel /
// DefaultMaxModel, config.DefaultTitleModel).

// Pinned slugs are EXACT model versions — deliberately NOT the
// `~`-prefixed OpenRouter floating aliases (`~google/gemini-flash-latest`
// etc.): the send-side reasoning reconstruction keys on the slug's
// family prefix, which the `~` sigil defeats, so thinking signatures get
// dropped across tool loops and Anthropic hard-400s with "Invalid
// `signature` in `thinking` block" (root-caused + live-verified
// 2026-06-04). Trade-off: lab refreshes require bumping these constants
// — and their server-side mirrors — instead of floating automatically.
export const DEFAULT_MODEL = "deepseek/deepseek-v4-flash-0731";
export const DEFAULT_MODEL_LABEL = "DeepSeek: DeepSeek V4 Flash 0731";

export const ADVANCED_MODEL = "x-ai/grok-4.6";
export const ADVANCED_MODEL_LABEL = "SpaceXAI: Grok 4.6";

// TIER_MODELS is the ordered list the picker pins to the top of the
// dropdown when no search query is active. Rows render their display
// names; the picker adds the "recommended" pill.
export const TIER_MODELS: ReadonlyArray<{ slug: string; label: string }> = [
  { slug: DEFAULT_MODEL, label: DEFAULT_MODEL_LABEL },
  { slug: ADVANCED_MODEL, label: ADVANCED_MODEL_LABEL },
];

// TESTED_MODELS lists slugs we've validated end-to-end against our
// tool catalog and system prompt but don't pin to the top of the
// picker. Anything not in this set and not a pinned slug is treated as
// "experimental" — it should still work, but we haven't checked.
const TESTED_MODELS: ReadonlySet<string> = new Set([
  "openai/gpt-5.4",
]);

// ModelTier keys are INTERNAL badge categories (the visible pill for the
// two pinned rows reads "recommended"); "default"/"advanced" survive as
// key names only to keep the badge plumbing stable.
export type ModelTier = "default" | "advanced" | "tested" | "experimental";

// labelForModel returns the display name for a pinned slug, or the raw
// slug otherwise. Used by the model-picker chip + dropdown so the two
// pinned models read as real model names, never as aliases.
export function labelForModel(slug: string): string {
  if (slug === DEFAULT_MODEL) return DEFAULT_MODEL_LABEL;
  if (slug === ADVANCED_MODEL) return ADVANCED_MODEL_LABEL;
  return slug;
}

// tierForModel classifies a slug into a UI badge category. Pinned slugs
// get their slot key (rendered as the "recommended" pill); everything
// else is "tested" or "experimental".
export function tierForModel(slug: string): ModelTier {
  if (slug === DEFAULT_MODEL) return "default";
  if (slug === ADVANCED_MODEL) return "advanced";
  if (TESTED_MODELS.has(slug)) return "tested";
  return "experimental";
}
