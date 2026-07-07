// Catwalk model catalog cache.
//
// Catwalk (github.com/charmbracelet/catwalk) is the community-maintained,
// no-auth model database Charm's Crush uses to know which models each LLM
// provider serves. We use it for the one thing the direct provider APIs
// can't give us without an API key: the list of models behind an
// admin-configured *catch-all* workspace provider (Settings → Admin → Model
// providers with an empty models list). The browser never talks to catwalk
// directly (CSP pins connect-src to 'self'); this module runs in the Next
// server and /api/catwalk-models proxies a trimmed view of it.
//
// Advisory only: a stale or unreachable catwalk degrades to an empty list —
// pickers still accept free-typed slugs, and nothing at run time depends on
// this catalog.

// Same default base + path as crush (internal/config/load.go
// defaultCatwalkURL + pkg/catwalk client's "/v2/providers"). CATWALK_URL
// mirrors crush's env override so air-gapped deployments can point at a
// self-hosted catwalk.
const CATWALK_BASE_URL = process.env.CATWALK_URL?.trim() || "https://catwalk.charm.land";
export const CATWALK_PROVIDERS_URL = `${CATWALK_BASE_URL.replace(/\/+$/, "")}/v2/providers`;

export type CatwalkModel = {
  id: string;
  name: string;
  contextWindow?: number;
  // USD per 1M output tokens — same axis the OpenRouter picker sorts on.
  costPer1MOut?: number;
};

export type CatwalkProvider = {
  // Catwalk's provider id ("anthropic", "openai", …). Fleet's workspace
  // provider *types* use the same vocabulary for the first-party labs, which
  // is what lets the picker expand a catch-all by type.
  id: string;
  name: string;
  type: string;
  models: CatwalkModel[];
};

type RawModel = {
  id?: unknown;
  name?: unknown;
  context_window?: unknown;
  cost_per_1m_out?: unknown;
};

type RawProvider = {
  id?: unknown;
  name?: unknown;
  type?: unknown;
  models?: unknown;
};

const CACHE_TTL_MS = 24 * 60 * 60 * 1000;

let cached: { providers: CatwalkProvider[]; fetchedAt: number } | null = null;
let inflight: Promise<CatwalkProvider[]> | null = null;

function parseNumber(raw: unknown): number | undefined {
  if (typeof raw === "number" && Number.isFinite(raw) && raw > 0) return raw;
  return undefined;
}

export function parseCatwalkProviders(payload: unknown): CatwalkProvider[] {
  if (!Array.isArray(payload)) {
    throw new Error("catwalk providers response is not an array");
  }
  const out: CatwalkProvider[] = [];
  for (const raw of payload as RawProvider[]) {
    const id = typeof raw?.id === "string" ? raw.id.trim() : "";
    if (!id) continue;
    const models: CatwalkModel[] = [];
    for (const m of Array.isArray(raw.models) ? (raw.models as RawModel[]) : []) {
      const mid = typeof m?.id === "string" ? m.id.trim() : "";
      if (!mid) continue;
      models.push({
        id: mid,
        name: typeof m?.name === "string" && m.name.trim() ? m.name.trim() : mid,
        contextWindow: parseNumber(m?.context_window),
        costPer1MOut: parseNumber(m?.cost_per_1m_out),
      });
    }
    out.push({
      id,
      name: typeof raw?.name === "string" && raw.name.trim() ? raw.name.trim() : id,
      type: typeof raw?.type === "string" ? raw.type.trim() : "",
      models,
    });
  }
  if (out.length === 0) {
    throw new Error("catwalk providers response contained no usable entries");
  }
  return out;
}

export async function loadCatwalkProviders(): Promise<CatwalkProvider[]> {
  if (cached && Date.now() - cached.fetchedAt < CACHE_TTL_MS) {
    return cached.providers;
  }
  if (inflight) return inflight;
  inflight = (async () => {
    try {
      const res = await fetch(CATWALK_PROVIDERS_URL, {
        headers: { Accept: "application/json" },
        cache: "no-store",
        signal: AbortSignal.timeout(8_000),
      });
      if (!res.ok) {
        throw new Error(`catwalk providers fetch failed: ${res.status}`);
      }
      const providers = parseCatwalkProviders((await res.json()) as unknown);
      cached = { providers, fetchedAt: Date.now() };
      return providers;
    } finally {
      inflight = null;
    }
  })();
  return inflight;
}

// Test helper: reset the module cache (fetch is mocked in unit tests).
export function __resetCatwalkCacheForTests() {
  cached = null;
  inflight = null;
}
