// Frontend model-tier slugs.
//
// The chat-server has no concept of a "default" model — every /chat
// request carries the slug the UI resolved. What lives here is the two
// ROLE slots the UI pins:
//   - the DEFAULT tier — what a new conversation starts on (shown with
//     the "recommended" pill in the picker)
//   - the ADVANCED tier — the stronger escalation target: the model the
//     agent's suggest_advanced_model card and the spreadsheet nudge
//     offer to switch to (also pinned in the picker)
//
// Since #1187 the EFFECTIVE slugs are admin-configurable (Settings →
// Admin → Features → Model tiers) and arrive with /api/client-config on
// every authenticated shell mount; useClientConfig calls setModelTiers()
// as they land. The constants below are the compiled-in FALLBACK —
// what renders before the first config fetch resolves and what a bare
// deployment runs on — and they mirror the server seeds
// (agentcore/models.go → DefaultCoreModel / DefaultMaxModel). Read the
// live values through currentDefaultModel()/currentAdvancedModel()/
// currentTierModels(); import the constants only to reference the
// shipped defaults themselves.
//
// Beyond the two pinned slots we classify every other slug as either
// "tested" (we've validated it works end-to-end with our tools and
// system prompt) or "experimental" (anything else the user types in).

// Pinned slugs are EXACT model versions — deliberately NOT the
// `~`-prefixed OpenRouter floating aliases (`~google/gemini-flash-latest`
// etc.): the send-side reasoning reconstruction keys on the slug's
// family prefix, which the `~` sigil defeats, so thinking signatures get
// dropped across tool loops and Anthropic hard-400s with "Invalid
// `signature` in `thinking` block" (root-caused + live-verified
// 2026-06-04).
export const DEFAULT_MODEL = "google/gemini-3.7-flash";
export const DEFAULT_MODEL_LABEL = "Google: Gemini 3.7 Flash";

export const ADVANCED_MODEL = "openai/gpt-5.6-sol";
export const ADVANCED_MODEL_LABEL = "OpenAI: GPT-5.6 Sol";

// Display names for slugs we know by heart. An admin-configured tier
// slug outside this map renders as itself — honest, and the pickers'
// catalog rows still carry the upstream display name where one exists.
const KNOWN_LABELS: Readonly<Record<string, string>> = {
  [DEFAULT_MODEL]: DEFAULT_MODEL_LABEL,
  [ADVANCED_MODEL]: ADVANCED_MODEL_LABEL,
};

// The live tier pair. Module scope, same lifecycle as useClientConfig's
// module cache: seeded with the compiled-in constants, replaced when a
// /api/client-config payload lands, and surviving route remounts. There
// is no subscription mechanism — the config fetch that changes this also
// re-renders the chat shell, so render-time reads stay fresh.
let liveTiers = { defaultModel: DEFAULT_MODEL, advancedModel: ADVANCED_MODEL };

export type ModelTiersConfig = {
  default_model?: string;
  advanced_model?: string;
};

// setModelTiers installs the workspace's effective tier slugs (from the
// member-gated /client-config payload). Empty/missing fields keep the
// compiled-in fallback, so a payload from an older server changes nothing.
export function setModelTiers(cfg: ModelTiersConfig | null | undefined): void {
  const def = String(cfg?.default_model ?? "").trim();
  const adv = String(cfg?.advanced_model ?? "").trim();
  liveTiers = {
    defaultModel: def || DEFAULT_MODEL,
    advancedModel: adv || ADVANCED_MODEL,
  };
}

export function currentDefaultModel(): string {
  return liveTiers.defaultModel;
}

export function currentAdvancedModel(): string {
  return liveTiers.advancedModel;
}

export function currentDefaultModelLabel(): string {
  return labelForModel(liveTiers.defaultModel);
}

export function currentAdvancedModelLabel(): string {
  return labelForModel(liveTiers.advancedModel);
}

// currentTierModels is the ordered pair the picker pins to the top of the
// dropdown when no search query is active. Rows render their display
// names; the picker adds the "recommended" pill.
export function currentTierModels(): Array<{ slug: string; label: string }> {
  return [
    { slug: liveTiers.defaultModel, label: labelForModel(liveTiers.defaultModel) },
    { slug: liveTiers.advancedModel, label: labelForModel(liveTiers.advancedModel) },
  ];
}

// TIER_MODELS is the compiled-in pair — the pre-bootstrap fallback and the
// exclusion list server-side code (model-rankings) uses where no browser
// bootstrap exists. Live surfaces use currentTierModels().
export const TIER_MODELS: ReadonlyArray<{ slug: string; label: string }> = [
  { slug: DEFAULT_MODEL, label: DEFAULT_MODEL_LABEL },
  { slug: ADVANCED_MODEL, label: ADVANCED_MODEL_LABEL },
];

// _resetModelTiersForTests restores the compiled-in pair between tests.
export function _resetModelTiersForTests(): void {
  liveTiers = { defaultModel: DEFAULT_MODEL, advancedModel: ADVANCED_MODEL };
}

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

// labelForModel returns the display name for a slug we know, or the raw
// slug otherwise. Used by the model-picker chip + dropdown so the pinned
// models read as real model names, never as aliases.
export function labelForModel(slug: string): string {
  return KNOWN_LABELS[slug] ?? slug;
}

// tierForModel classifies a slug into a UI badge category against the
// LIVE tier pair. Pinned slugs get their slot key (rendered as the
// "recommended" pill); everything else is "tested" or "experimental".
export function tierForModel(slug: string): ModelTier {
  if (slug === liveTiers.defaultModel) return "default";
  if (slug === liveTiers.advancedModel) return "advanced";
  if (TESTED_MODELS.has(slug)) return "tested";
  return "experimental";
}
