// Workspace-provider models for the model pickers (chat + task form).
//
// Admin-configured providers (Settings → Admin → Model providers) contribute
// pickable models two ways:
//   1. Providers with an explicit models list — the Go /llm-provider-models
//      read returns one "<provider>/<model>" slug per listed model.
//   2. Catch-all providers (empty models list, they serve any slug) — the Go
//      read enumerates nothing, so we expand known first-party types
//      (anthropic, openai) from the catwalk model database via
//      /api/catwalk-models. The expansion is advisory: it only seeds the
//      picker; any free-typed "<provider>/<model>" slug still routes.
//
// Both fetches are same-origin session calls; any failure degrades to an
// empty list so the pickers are never blocked on this module.

export type WorkspaceModel = {
  // The slug to send as the turn/task model: "<provider>/<model>".
  id: string;
  name: string;
  contextLength?: number;
};

type ProviderModelsResponse = {
  models?: Array<{ id?: unknown; name?: unknown }>;
  providers?: Array<{ name?: unknown; type?: unknown; catch_all?: unknown }>;
};

type CatwalkResponse = {
  providers?: Array<{
    id?: unknown;
    models?: Array<{ id?: unknown; name?: unknown; context_window?: unknown }>;
  }>;
};

// Fleet provider types whose catch-all rows we can expand from catwalk. The
// fleet type vocabulary matches catwalk's first-party provider ids. Other
// types stay unexpanded: "openrouter" is already covered by the OpenRouter
// catalog fetch, and "ollama" serves whatever the local daemon has pulled —
// no public catalog can know that.
const CATWALK_EXPANDABLE_TYPES = new Set(["anthropic", "openai"]);

let cachedModels: WorkspaceModel[] | null = null;
let inflight: Promise<WorkspaceModel[]> | null = null;

async function fetchJSON<T>(url: string): Promise<T | null> {
  try {
    const response = await fetch(url, { cache: "no-store" });
    if (!response.ok) return null;
    return (await response.json()) as T;
  } catch {
    return null;
  }
}

async function fetchWorkspaceModelsOnce(): Promise<WorkspaceModel[]> {
  const payload = await fetchJSON<ProviderModelsResponse>("/api/llm-provider-models");
  if (!payload) return [];

  const out: WorkspaceModel[] = [];
  const seen = new Set<string>();
  for (const m of Array.isArray(payload.models) ? payload.models : []) {
    const id = String(m?.id ?? "").trim();
    if (!id || seen.has(id)) continue;
    seen.add(id);
    out.push({ id, name: String(m?.name ?? id).trim() });
  }

  // Expand catch-all providers of known types from catwalk. One catalog
  // fetch serves every expandable provider.
  const catchAlls = (Array.isArray(payload.providers) ? payload.providers : []).filter(
    (p) =>
      p?.catch_all === true &&
      CATWALK_EXPANDABLE_TYPES.has(String(p?.type ?? "").trim().toLowerCase()) &&
      String(p?.name ?? "").trim() !== "",
  );
  if (catchAlls.length > 0) {
    const catwalk = await fetchJSON<CatwalkResponse>("/api/catwalk-models");
    const byId = new Map(
      (Array.isArray(catwalk?.providers) ? catwalk.providers : []).map((p) => [
        String(p?.id ?? "").trim().toLowerCase(),
        p,
      ]),
    );
    for (const p of catchAlls) {
      const providerName = String(p.name).trim();
      const catalog = byId.get(String(p.type).trim().toLowerCase());
      for (const m of Array.isArray(catalog?.models) ? catalog.models : []) {
        const modelId = String(m?.id ?? "").trim();
        if (!modelId) continue;
        const slug = `${providerName}/${modelId}`;
        if (seen.has(slug)) continue;
        seen.add(slug);
        const contextWindow = Number(m?.context_window);
        out.push({
          id: slug,
          name: `${providerName}: ${String(m?.name ?? modelId).trim()}`,
          contextLength:
            Number.isFinite(contextWindow) && contextWindow > 0 ? contextWindow : undefined,
        });
      }
    }
  }
  return out;
}

// Returns the workspace-provider model list, fetched once per page load and
// cached. Never rejects — failures resolve to [].
export async function loadWorkspaceModels(): Promise<WorkspaceModel[]> {
  if (cachedModels) return cachedModels;
  if (inflight) return inflight;
  inflight = fetchWorkspaceModelsOnce()
    .then((models) => {
      cachedModels = models;
      return models;
    })
    .finally(() => {
      inflight = null;
    });
  return inflight;
}

// Test seam: clears the module cache between cases.
export function _resetWorkspaceModelCacheForTests() {
  cachedModels = null;
  inflight = null;
}
