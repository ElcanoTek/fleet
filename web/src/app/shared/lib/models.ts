// OpenRouter model catalog logic for the orchestrator task form's model
// picker. Pure functions ported from moc's assets/js/model-picker.js — the
// DOM/positioning machinery is rebuilt as a React component (ModelPicker.tsx);
// this module is the testable filtering/affordability core.

export type PickerModel = {
  id: string;
  name: string;
  recommended: boolean;
  created?: number | null;
  priceCompletion?: number | null;
};

type RawModel = {
  id?: unknown;
  name?: unknown;
  created?: unknown;
  pricing?: { prompt?: unknown; completion?: unknown };
  architecture?: { output_modalities?: unknown; modality?: unknown };
};

export const OPENROUTER_MODELS_URL = "https://openrouter.ai/api/v1/models";

// Per-token price ceilings ($/token). 8e-6 = $8 per million tokens.
const MAX_PROMPT_PRICE = 8e-6;
const MAX_COMPLETION_PRICE = 30e-6;

const REQUEST_TIMEOUT_MS = 8000;
export const MAX_RESULTS = 50;

// Hand-picked entries shown immediately and used as a fallback when the
// OpenRouter fetch fails. Pinned release slugs (not floating `~` aliases).
export const SEED_MODELS: PickerModel[] = [
  { id: "anthropic/claude-opus-4.8", name: "Anthropic: Claude Opus 4.8", recommended: true },
  { id: "moonshotai/kimi-k2.6", name: "MoonshotAI: Kimi K2.6", recommended: true },
  { id: "google/gemini-3.5-flash", name: "Google: Gemini 3.5 Flash", recommended: true },
];

export function isAffordable(model: RawModel): boolean {
  const prompt = Number.parseFloat(String(model?.pricing?.prompt ?? ""));
  const completion = Number.parseFloat(String(model?.pricing?.completion ?? ""));
  if (!Number.isFinite(prompt) || !Number.isFinite(completion)) return false;
  return prompt <= MAX_PROMPT_PRICE && completion <= MAX_COMPLETION_PRICE;
}

export function isTextOutput(model: RawModel): boolean {
  const arch = model?.architecture;
  if (!arch) return false;
  const outs = Array.isArray(arch.output_modalities) ? (arch.output_modalities as unknown[]) : null;
  if (outs) return outs.includes("text");
  const modality = String(arch.modality || "");
  if (!modality) return false;
  if (modality === "text") return true;
  const arrowIdx = modality.indexOf("->");
  if (arrowIdx === -1) return false;
  const outputs = modality
    .slice(arrowIdx + 2)
    .split("+")
    .map((s) => s.trim());
  return outputs.includes("text");
}

export function normaliseModel(raw: RawModel): PickerModel | null {
  const id = String(raw?.id ?? "").trim();
  if (!id) return null;
  const name = String(raw?.name ?? id).trim();
  const created =
    typeof raw?.created === "number" && Number.isFinite(raw.created) ? (raw.created as number) : null;
  const completion = Number.parseFloat(String(raw?.pricing?.completion ?? ""));
  const priceCompletion = Number.isFinite(completion) ? completion : null;
  return { id, name, recommended: false, created, priceCompletion };
}

export function dedupeAndOrder(seedList: PickerModel[], fetchedList: PickerModel[]): PickerModel[] {
  const seen = new Set<string>();
  const out: PickerModel[] = [];
  for (const list of [seedList, fetchedList]) {
    for (const m of list) {
      if (!m?.id || seen.has(m.id)) continue;
      seen.add(m.id);
      out.push(m);
    }
  }
  return out;
}

export function scoreMatch(model: PickerModel, query: string): number {
  const id = model.id.toLowerCase();
  const name = model.name.toLowerCase();
  if (id === query) return 1000;
  if (id.startsWith(query)) return 500;
  if (name.startsWith(query)) return 400;
  if (id.includes(query)) return 200;
  if (name.includes(query)) return 100;
  return 0;
}

export function filterModels(models: PickerModel[], query: string): PickerModel[] {
  const q = (query || "").trim().toLowerCase();
  if (!q) return models.slice(0, MAX_RESULTS);
  return models
    .map((model) => ({ model, score: scoreMatch(model, q) }))
    .filter((entry) => entry.score > 0)
    .sort((a, b) => {
      if (b.score !== a.score) return b.score - a.score;
      const aRec = a.model.recommended ? 1 : 0;
      const bRec = b.model.recommended ? 1 : 0;
      if (aRec !== bRec) return bRec - aRec;
      const aCreated = a.model.created ?? -Infinity;
      const bCreated = b.model.created ?? -Infinity;
      if (aCreated !== bCreated) return bCreated - aCreated;
      const aPrice = a.model.priceCompletion ?? -Infinity;
      const bPrice = b.model.priceCompletion ?? -Infinity;
      if (aPrice !== bPrice) return bPrice - aPrice;
      return a.model.id.localeCompare(b.model.id);
    })
    .slice(0, MAX_RESULTS)
    .map((entry) => entry.model);
}

async function fetchOpenRouterModels(): Promise<PickerModel[]> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS);
  try {
    const response = await fetch(OPENROUTER_MODELS_URL, {
      signal: controller.signal,
      cache: "no-store",
      credentials: "omit",
    });
    if (!response.ok) throw new Error(`status ${response.status}`);
    const payload = await response.json();
    const data: RawModel[] = Array.isArray(payload?.data) ? payload.data : [];
    return data
      .filter(isAffordable)
      .filter(isTextOutput)
      .map(normaliseModel)
      .filter((m): m is PickerModel => m !== null);
  } finally {
    clearTimeout(timer);
  }
}

let cachedModels: PickerModel[] | null = null;
let inflight: Promise<PickerModel[]> | null = null;

// Returns the merged (seed + OpenRouter) list, fetched once and cached. Falls
// back to the seeds alone when the network call fails so callers always have
// something to render.
export async function loadModels(): Promise<PickerModel[]> {
  if (cachedModels) return cachedModels;
  if (inflight) return inflight;
  inflight = (async () => {
    try {
      const fetched = await fetchOpenRouterModels();
      cachedModels = dedupeAndOrder(SEED_MODELS, fetched);
    } catch {
      cachedModels = SEED_MODELS.slice();
    } finally {
      inflight = null;
    }
    return cachedModels;
  })();
  return inflight;
}

// Test seam: clears the in-module cache so tests can reseed with mocked fetch
// responses without state bleed between cases.
export function _resetModelCacheForTests() {
  cachedModels = null;
  inflight = null;
}
