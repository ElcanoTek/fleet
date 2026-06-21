// OpenRouter model catalog + pricing cache.
//
// The /api/v1/models endpoint is public (no key needed). We pull it once a
// day, index by slug, and expose helpers so both the rankings dropdown and
// the custom-slug input can enforce a $30-per-million-output-token ceiling.
//
// $30 is deliberately a few dollars above the advanced tier (Opus-latest at
// $25/M output) so Opus is comfortably inside the ceiling rather than sitting
// exactly on it — a minor upstream price nudge shouldn't silently re-block it.
// The genuinely premium tiers stay out: Opus-4/4.1 ($75/M) and the *-fast
// variants ($50-150/M).

export const MAX_COMPLETION_USD_PER_MILLION = 30;
// pricing.completion on the OpenRouter models endpoint is USD per single token.
export const MAX_COMPLETION_USD_PER_TOKEN = MAX_COMPLETION_USD_PER_MILLION / 1_000_000;
export const MODELS_PAGE_URL = "https://openrouter.ai/models";
export const MODELS_API_URL = "https://openrouter.ai/api/v1/models";

export type CatalogEntry = {
  slug: string;
  canonicalSlug?: string;
  name: string;
  completionPerToken: number;
  promptPerToken: number;
  outputModalities: string[];
  // OpenRouter exposes `context_length` per model — total tokens the
  // provider accepts (prompt + completion combined). Optional because
  // the field is occasionally missing from third-party listings; the
  // UI degrades gracefully when undefined.
  contextLength?: number;
  // Unix timestamp (seconds) the model was first listed on OpenRouter.
  // Drives the "newest per lab" curation in listLatestPerLab. Optional
  // because legacy entries occasionally omit it.
  created?: number;
};

// Slug-prefix groups for the curated picker. The rankings endpoint
// emits one entry per lab here — the newest within-budget text-only
// model, with tier slugs filtered out so each row adds something the
// tier slots don't already cover. Order is the order the picker shows
// them in. Meta and Cohere are intentionally omitted.
export const MAJOR_LABS: ReadonlyArray<{ prefix: string; name: string }> = [
  { prefix: "anthropic", name: "Anthropic" },
  { prefix: "openai", name: "OpenAI" },
  { prefix: "google", name: "Google" },
  { prefix: "x-ai", name: "xAI" },
  { prefix: "deepseek", name: "DeepSeek" },
  { prefix: "moonshotai", name: "Moonshot" },
  { prefix: "qwen", name: "Qwen" },
  { prefix: "mistralai", name: "Mistral" },
  { prefix: "nvidia", name: "NVIDIA" },
];

export type Catalog = {
  // Keyed by the short, stable id (e.g. "moonshotai/kimi-k2.6"). This is
  // what the catalog calls a model's primary slug.
  entries: Map<string, CatalogEntry>;
  // Keyed by either the short id OR the dated canonical_slug (e.g.
  // "moonshotai/kimi-k2.6-20260420"). The rankings HTML uses canonical
  // slugs, so we match against this map and then emit the short id back
  // to the client for a consistent selection value.
  bySlug: Map<string, CatalogEntry>;
  fetchedAt: number;
};

// Resolve either form of slug to the catalog entry. Prefer this over
// reaching into `entries` directly when the input could be a canonical
// slug from the rankings page.
export function resolveSlug(catalog: Catalog, slug: string): CatalogEntry | undefined {
  return catalog.bySlug.get(slug);
}

export type ValidateResult =
  | { ok: true; entry?: CatalogEntry }
  | {
      ok: false;
      reason: "empty" | "over_budget";
      message: string;
      modelsUrl: string;
      entry?: CatalogEntry;
    };

const CACHE_TTL_MS = 24 * 60 * 60 * 1000;

let cached: Catalog | null = null;
let inflight: Promise<Catalog> | null = null;

type RawModel = {
  id?: unknown;
  canonical_slug?: unknown;
  name?: unknown;
  context_length?: unknown;
  created?: unknown;
  pricing?: { prompt?: unknown; completion?: unknown } | null;
  architecture?: { output_modalities?: unknown } | null;
};

function parseContextLength(raw: unknown): number | undefined {
  if (typeof raw === "number" && Number.isFinite(raw) && raw > 0) {
    return Math.floor(raw);
  }
  if (typeof raw === "string") {
    const n = Number(raw);
    if (Number.isFinite(n) && n > 0) return Math.floor(n);
  }
  return undefined;
}

function parseCreated(raw: unknown): number | undefined {
  if (typeof raw === "number" && Number.isFinite(raw) && raw > 0) {
    return Math.floor(raw);
  }
  if (typeof raw === "string") {
    const n = Number(raw);
    if (Number.isFinite(n) && n > 0) return Math.floor(n);
  }
  return undefined;
}

export function parsePrice(raw: unknown): number {
  if (typeof raw === "number" && Number.isFinite(raw)) return raw;
  if (typeof raw === "string" && raw.trim() !== "") {
    const n = Number(raw);
    return Number.isFinite(n) ? n : Number.NaN;
  }
  return Number.NaN;
}

function buildCatalog(payload: unknown): Catalog {
  const entries = new Map<string, CatalogEntry>();
  const bySlug = new Map<string, CatalogEntry>();
  const data = (payload as { data?: unknown })?.data;
  if (!Array.isArray(data)) {
    throw new Error("openrouter models response missing data[] array");
  }
  for (const raw of data as RawModel[]) {
    if (!raw || typeof raw.id !== "string") continue;
    const completion = parsePrice(raw.pricing?.completion);
    const prompt = parsePrice(raw.pricing?.prompt);
    // Skip entries without a numeric completion price — we cannot reason
    // about the budget for them. OpenRouter uses "0" for free tiers, which
    // is a real number and passes through.
    if (!Number.isFinite(completion)) continue;
    const outputModalities = Array.isArray(raw.architecture?.output_modalities)
      ? (raw.architecture.output_modalities.filter((x): x is string => typeof x === "string"))
      : [];
    const canonicalSlug =
      typeof raw.canonical_slug === "string" && raw.canonical_slug !== raw.id
        ? raw.canonical_slug
        : undefined;
    const entry: CatalogEntry = {
      slug: raw.id,
      canonicalSlug,
      name: typeof raw.name === "string" ? raw.name : raw.id,
      completionPerToken: completion,
      promptPerToken: Number.isFinite(prompt) ? prompt : 0,
      outputModalities,
      contextLength: parseContextLength(raw.context_length),
      created: parseCreated(raw.created),
    };
    entries.set(raw.id, entry);
    bySlug.set(raw.id, entry);
    if (canonicalSlug) bySlug.set(canonicalSlug, entry);
  }
  if (entries.size === 0) {
    throw new Error("openrouter models response contained no usable entries");
  }
  return { entries, bySlug, fetchedAt: Date.now() };
}

export async function loadCatalog(): Promise<Catalog> {
  if (cached && Date.now() - cached.fetchedAt < CACHE_TTL_MS) {
    return cached;
  }
  if (inflight) return inflight;
  inflight = (async () => {
    try {
      const res = await fetch(MODELS_API_URL, {
        headers: {
          "User-Agent": "Elcano Chat/1.0",
          Accept: "application/json",
        },
        cache: "no-store",
        // Upstream timeout. OpenRouter typically answers in <500ms; 5s is a
        // comfortable ceiling that still protects /api/chat from hanging if the
        // catalog endpoint stalls without returning an error.
        signal: AbortSignal.timeout(5_000),
      });
      if (!res.ok) {
        throw new Error(`openrouter models fetch failed: ${res.status}`);
      }
      const payload = (await res.json()) as unknown;
      const built = buildCatalog(payload);
      cached = built;
      return built;
    } finally {
      inflight = null;
    }
  })();
  return inflight;
}

export function isWithinBudget(entry: CatalogEntry): boolean {
  return entry.completionPerToken <= MAX_COMPLETION_USD_PER_TOKEN;
}

// Text-completion models emit text tokens ONLY. Every model on
// OpenRouter that declares an image or audio output today is a
// dedicated generator (Nano Banana, GPT Image, GPT Audio, Lyria, the
// Auto Router's image-routing variant, etc.), not a chat model that
// occasionally emits a different modality. The chat UI can't render
// those outputs, so we require `output_modalities === ["text"]`.
// Entries with an unknown/missing modalities list pass through, since
// the field is a recent addition and missing it shouldn't drop legacy
// text models from the picker.
export function isTextCompletion(entry: CatalogEntry): boolean {
  if (entry.outputModalities.length === 0) return true;
  return (
    entry.outputModalities.includes("text") &&
    entry.outputModalities.every((m) => m === "text")
  );
}

// Entries the picker is allowed to offer: within the completion-price
// ceiling and capable of text output. Sorted by completion price
// descending — expensive (but still under $25/M) surfaces first as a
// proxy for quality — with a stable slug tiebreak.
export function listAllowed(catalog: Catalog): CatalogEntry[] {
  const out: CatalogEntry[] = [];
  for (const entry of catalog.entries.values()) {
    if (isWithinBudget(entry) && isTextCompletion(entry)) out.push(entry);
  }
  out.sort((a, b) => {
    if (b.completionPerToken !== a.completionPerToken) {
      return b.completionPerToken - a.completionPerToken;
    }
    return a.slug.localeCompare(b.slug);
  });
  return out;
}

// Curated cross-lab picker. For each lab in MAJOR_LABS, returns the
// newest within-budget text-only model whose slug isn't in
// `excludeSlugs`. Sort within a lab is `created` desc; ties (or
// missing created) fall back to completion price desc as a flagship
// proxy. Labs with no qualifying entry are omitted, so the result
// length is at most MAJOR_LABS.length. Output order matches MAJOR_LABS.
//
// `excludeSlugs` is the set of tier slugs the UI already pins at the
// top of the picker — passing them in here means the rankings list
// expands the user's choices instead of duplicating tier rows.
export function listLatestPerLab(
  catalog: Catalog,
  excludeSlugs: ReadonlyArray<string> = [],
): CatalogEntry[] {
  const exclude = new Set(excludeSlugs);
  const out: CatalogEntry[] = [];
  for (const { prefix } of MAJOR_LABS) {
    const labPrefix = `${prefix}/`;
    let best: CatalogEntry | undefined;
    for (const entry of catalog.entries.values()) {
      if (!entry.slug.startsWith(labPrefix)) continue;
      if (exclude.has(entry.slug)) continue;
      // Skip OpenRouter's `:free` tier variants. They're typically the
      // same weights as a paid sibling but with stricter rate limits +
      // smaller context windows; surfacing them as the lab's "newest"
      // pick would mislead users into a degraded experience when the
      // paid version would be a few cents per million.
      if (entry.slug.endsWith(":free")) continue;
      if (!isWithinBudget(entry)) continue;
      if (!isTextCompletion(entry)) continue;
      if (!best) {
        best = entry;
        continue;
      }
      const a = entry.created ?? 0;
      const b = best.created ?? 0;
      if (a > b) {
        best = entry;
      } else if (a === b && entry.completionPerToken > best.completionPerToken) {
        best = entry;
      }
    }
    if (best) out.push(best);
  }
  return out;
}

export function overBudgetMessage(slug: string, entry: CatalogEntry): string {
  const perMillion = entry.completionPerToken * 1_000_000;
  return (
    `Model "${slug}" costs $${perMillion.toFixed(2)} per million output tokens, ` +
    `which is over the $${MAX_COMPLETION_USD_PER_MILLION} limit enforced by this chat. ` +
    `Pick a different model from ${MODELS_PAGE_URL}.`
  );
}

// Validate a user-supplied slug against the catalog.
//
// - Empty → rejected as "empty".
// - Present in catalog and over budget → rejected as "over_budget" with a
//   user-facing message and a link back to the OpenRouter models page.
// - Present in catalog and within budget → ok with the entry.
// - Not in catalog (unknown/new slug) → ok without an entry. We only block
//   models we can prove are over budget; unknown slugs pass through so
//   newly released models keep working if the cache is stale.
export async function validateSlug(slug: string): Promise<ValidateResult> {
  const trimmed = slug.trim();
  if (!trimmed) {
    return {
      ok: false,
      reason: "empty",
      message: "Model slug is required.",
      modelsUrl: MODELS_PAGE_URL,
    };
  }
  const catalog = await loadCatalog();
  const entry = resolveSlug(catalog, trimmed);
  if (!entry) {
    return { ok: true };
  }
  if (!isWithinBudget(entry)) {
    return {
      ok: false,
      reason: "over_budget",
      message: overBudgetMessage(trimmed, entry),
      modelsUrl: MODELS_PAGE_URL,
      entry,
    };
  }
  return { ok: true, entry };
}

// Test helper: reset the cache. We mock globalThis.fetch in unit tests to
// provide a fabricated catalog. Not exported from a barrel — imported directly
// by tests.
export function __resetCatalogForTests() {
  cached = null;
  inflight = null;
}

export const __buildCatalogForTests = buildCatalog;
