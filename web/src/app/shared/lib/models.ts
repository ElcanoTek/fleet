// Model catalog logic for the orchestrator task form's model picker. Pure
// functions ported from moc's assets/js/model-picker.js — the DOM/positioning
// machinery is rebuilt as a React component (ModelPicker.tsx); this module is
// the testable filtering/ordering core.
//
// The OpenRouter catalog is fetched through the session-gated same-origin
// proxy (/api/model-catalog) — NOT directly from openrouter.ai. The app's CSP
// pins connect-src to 'self' (#590), so a direct browser fetch is blocked and
// used to silently strand the picker on its two seed models.

import { loadWorkspaceModels, _resetWorkspaceModelCacheForTests } from "./workspaceModels";

export type PickerModel = {
  id: string;
  name: string;
  recommended: boolean;
  created?: number | null;
  priceCompletion?: number | null;
  contextLength?: number | null;
  // True for models served by an admin-configured workspace provider
  // (Settings → Admin → Model providers) rather than the OpenRouter catalog.
  workspace?: boolean;
};

export const MODEL_CATALOG_URL = "/api/model-catalog";

const REQUEST_TIMEOUT_MS = 8000;
export const MAX_RESULTS = 50;

// Hand-picked entries shown immediately and used as a fallback when the
// catalog fetch fails. Pinned release slugs (not floating `~` aliases).
export const SEED_MODELS: PickerModel[] = [
  { id: "z-ai/glm-5.2", name: "Z.AI: GLM 5.2", recommended: true },
  { id: "openai/gpt-5.6-sol", name: "OpenAI: GPT-5.6 Sol", recommended: true },
];

type RawCatalogModel = {
  slug?: unknown;
  name?: unknown;
  created?: unknown;
  context_length?: unknown;
  price_completion?: unknown;
};

function parseFiniteNumber(raw: unknown): number | null {
  const n = Number(raw);
  return Number.isFinite(n) ? n : null;
}

// normaliseCatalogModel maps one /api/model-catalog entry to a PickerModel.
// The proxy has already filtered to text-output models, so no modality
// checks remain client-side.
export function normaliseCatalogModel(raw: RawCatalogModel): PickerModel | null {
  const id = String(raw?.slug ?? "").trim();
  if (!id) return null;
  const name = String(raw?.name ?? id).trim();
  return {
    id,
    name,
    recommended: false,
    created: parseFiniteNumber(raw?.created),
    priceCompletion: parseFiniteNumber(raw?.price_completion),
    contextLength: parseFiniteNumber(raw?.context_length),
  };
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

async function fetchCatalogModels(): Promise<PickerModel[]> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS);
  try {
    const response = await fetch(MODEL_CATALOG_URL, {
      signal: controller.signal,
      cache: "no-store",
    });
    if (!response.ok) throw new Error(`status ${response.status}`);
    const payload = await response.json();
    const data: RawCatalogModel[] = Array.isArray(payload?.models) ? payload.models : [];
    return data.map(normaliseCatalogModel).filter((m): m is PickerModel => m !== null);
  } finally {
    clearTimeout(timer);
  }
}

let cachedModels: PickerModel[] | null = null;
let inflight: Promise<PickerModel[]> | null = null;

// Returns the merged (workspace + seed + OpenRouter-catalog) list, fetched
// once and cached. Workspace-provider models come first so an
// admin-configured model is immediately visible in browse mode; either fetch
// failing degrades to the rest of the list, and the seeds alone are the floor.
export async function loadModels(): Promise<PickerModel[]> {
  if (cachedModels) return cachedModels;
  if (inflight) return inflight;
  inflight = (async () => {
    const [workspaceRaw, fetched] = await Promise.all([
      loadWorkspaceModels(),
      fetchCatalogModels().catch(() => [] as PickerModel[]),
    ]);
    const workspace: PickerModel[] = workspaceRaw.map((m) => ({
      id: m.id,
      name: m.name,
      recommended: false,
      contextLength: m.contextLength ?? null,
      workspace: true,
    }));
    const base = fetched.length > 0 ? dedupeAndOrder(SEED_MODELS, fetched) : SEED_MODELS.slice();
    cachedModels = dedupeAndOrder(workspace, base);
    inflight = null;
    return cachedModels;
  })();
  return inflight;
}

// Test seam: clears the in-module cache so tests can reseed with mocked fetch
// responses without state bleed between cases.
export function _resetModelCacheForTests() {
  cachedModels = null;
  inflight = null;
  _resetWorkspaceModelCacheForTests();
}
