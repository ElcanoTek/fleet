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
export const DEFAULT_MODEL = "anthropic/claude-haiku-5.5";
export const DEFAULT_MODEL_LABEL = "Anthropic: Claude Haiku 5.5";

export const ADVANCED_MODEL = "anthropic/claude-opus-5.5";
export const ADVANCED_MODEL_LABEL = "Anthropic: Claude Opus 5.5";

// The scheduled-task FALLBACK the Operations Center create form pre-fills:
// what a task fails over to when its primary is down. Compiled-in fallback
// only — the effective slug is the operator's FLEET_TASK_FALLBACK_MODEL,
// which arrives as `task_fallback_model` on /api/client-config. Read it via
// currentTaskFallbackModel(). (The form's pre-filled PRIMARY is the live
// task tier, currentTaskModel(): the admin "Task model" setting, which
// follows the advanced tier unless overridden. Chat runs on the fast
// default tier; a scheduled task is a long unattended run.)
export const DEFAULT_TASK_FALLBACK_MODEL = "deepseek/deepseek-v4.1-flash";

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
let liveTiers = {
  defaultModel: DEFAULT_MODEL,
  advancedModel: ADVANCED_MODEL,
  taskModel: ADVANCED_MODEL,
  taskFallbackModel: DEFAULT_TASK_FALLBACK_MODEL,
};

export type ModelTiersConfig = {
  default_model?: string;
  advanced_model?: string;
  task_model?: string;
  task_fallback_model?: string;
  pinned?: unknown;
};

// PinnedModel is one operator-pinned picker row (FLEET_PINNED_MODELS, carried
// as `models.pinned` on /api/client-config): a slug both pickers list right
// under the recommended tiers, shown under `label` when set and with `tag` as
// its pill (e.g. "alpha"). `free`, or the optional prices (USD per million
// tokens), replace the catalog's price in the cost band. Presentation only — the slug is
// still what a turn or task sends and what the server resolves, and real
// spend is metered at the provider's price; see docs/PINNED-MODELS.md.
export type PinnedModel = {
  slug: string;
  label?: string;
  tag?: string;
  // Shown as "Free" in place of the cost band (display only).
  free?: boolean;
  inputPricePerMtok?: number;
  outputPricePerMtok?: number;
};

function parsePrice(raw: unknown): number | undefined {
  return typeof raw === "number" && Number.isFinite(raw) && raw >= 0 ? raw : undefined;
}

// pinnedPrices converts a pin's per-million overrides into the per-token
// pricePrompt / priceCompletion fields the picker rows carry; only the sides
// the operator set are returned, so the rest keep the catalog value.
export function pinnedPrices(
  pin: PinnedModel | undefined,
): { pricePrompt?: number; priceCompletion?: number; free?: boolean } {
  if (!pin) return {};
  if (pin.free) return { pricePrompt: 0, priceCompletion: 0, free: true };
  return {
    ...(pin.inputPricePerMtok !== undefined ? { pricePrompt: pin.inputPricePerMtok / 1e6 } : {}),
    ...(pin.outputPricePerMtok !== undefined ? { priceCompletion: pin.outputPricePerMtok / 1e6 } : {}),
  };
}

let livePinned: ReadonlyArray<PinnedModel> = [];

// parsePinned keeps only well-formed entries from the opaque payload, so an
// unexpected shape degrades to "no pins" rather than breaking the shell.
function parsePinned(raw: unknown): PinnedModel[] {
  if (!Array.isArray(raw)) return [];
  const seen = new Set<string>();
  const out: PinnedModel[] = [];
  for (const entry of raw) {
    const slug = String((entry as { slug?: unknown })?.slug ?? "").trim();
    if (!slug || seen.has(slug)) continue;
    seen.add(slug);
    const label = String((entry as { label?: unknown })?.label ?? "").trim();
    const tag = String((entry as { tag?: unknown })?.tag ?? "").trim();
    const input = parsePrice((entry as { input_price_per_mtok?: unknown })?.input_price_per_mtok);
    const output = parsePrice((entry as { output_price_per_mtok?: unknown })?.output_price_per_mtok);
    out.push({
      slug,
      ...(label ? { label } : {}),
      ...(tag ? { tag } : {}),
      ...((entry as { free?: unknown })?.free === true ? { free: true } : {}),
      ...(input !== undefined ? { inputPricePerMtok: input } : {}),
      ...(output !== undefined ? { outputPricePerMtok: output } : {}),
    });
  }
  return out;
}

// setModelTiers installs the workspace's effective tier slugs (from the
// member-gated /client-config payload). Empty/missing fields keep the
// compiled-in fallback, so a payload from an older server changes nothing.
export function setModelTiers(cfg: ModelTiersConfig | null | undefined): void {
  const def = String(cfg?.default_model ?? "").trim();
  const adv = String(cfg?.advanced_model ?? "").trim();
  const task = String(cfg?.task_model ?? "").trim();
  const fb = String(cfg?.task_fallback_model ?? "").trim();
  const advancedModel = adv || ADVANCED_MODEL;
  liveTiers = {
    defaultModel: def || DEFAULT_MODEL,
    advancedModel,
    // An older server sends no task_model; the task tier then follows the
    // advanced tier, which is what the server would have resolved anyway.
    taskModel: task || advancedModel,
    taskFallbackModel: fb || DEFAULT_TASK_FALLBACK_MODEL,
  };
  livePinned = parsePinned(cfg?.pinned);
}

// currentPinnedModels is the operator's pinned rows in declared order (empty
// until a /api/client-config payload carrying some lands).
export function currentPinnedModels(): ReadonlyArray<PinnedModel> {
  return livePinned;
}

// pinnedModelFor returns the pin for a slug, if the operator pinned it.
export function pinnedModelFor(slug: string): PinnedModel | undefined {
  return livePinned.find((p) => p.slug === slug);
}

// pinnedLabelFor returns the operator's display label for a pinned slug, or
// undefined when the slug is not pinned or the pin keeps the catalog name.
export function pinnedLabelFor(slug: string): string | undefined {
  return pinnedModelFor(slug)?.label;
}

// currentTaskFallbackModel is the fallback slug the task-create form pre-fills:
// the operator's scheduler fallback when one is configured, else the
// compiled-in DEFAULT_TASK_FALLBACK_MODEL.
export function currentTaskFallbackModel(): string {
  return liveTiers.taskFallbackModel;
}

// currentTaskModel is the primary model the task-create form pre-fills: the
// live task tier (the advanced tier unless an admin set a task model).
export function currentTaskModel(): string {
  return liveTiers.taskModel;
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
  livePinned = [];
  liveTiers = {
    defaultModel: DEFAULT_MODEL,
    advancedModel: ADVANCED_MODEL,
    taskModel: ADVANCED_MODEL,
    taskFallbackModel: DEFAULT_TASK_FALLBACK_MODEL,
  };
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
//
// An operator pin label (FLEET_PINNED_MODELS) wins over the compiled-in
// labels, so a relabelled slug reads the same everywhere it is named.
export function labelForModel(slug: string): string {
  return pinnedLabelFor(slug) ?? KNOWN_LABELS[slug] ?? slug;
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
