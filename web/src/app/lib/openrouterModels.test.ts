import { afterEach, describe, expect, it, vi } from "vitest";
import {
  MAX_COMPLETION_USD_PER_MILLION,
  MODELS_PAGE_URL,
  __resetCatalogForTests,
  __buildCatalogForTests,
  isTextCompletion,
  isWithinBudget,
  listAllowed,
  listLatestPerLab,
  loadCatalog,
  resolveSlug,
  validateSlug,
  parsePrice,
  type CatalogEntry,
} from "./openrouterModels";

// USD per single token — OpenRouter's /api/v1/models pricing shape.
const cheapEntry = {
  slug: "google/gemini-3-flash-preview",
  name: "Gemini 3 Flash",
  completionPerToken: 0.0000005,
  promptPerToken: 0.0000001,
  outputModalities: ["text"],
};
const atCeilingEntry = {
  slug: "acme/at-ceiling",
  name: "At Ceiling",
  // exactly $30 per million — must be allowed (inclusive).
  completionPerToken: 30 / 1_000_000,
  promptPerToken: 0,
  outputModalities: ["text"],
};
// Regression guard for the advanced tier: Opus 4.8 is $25/M output and
// MUST stay within budget. This is the "advanced" tier slug, and a too-low
// ceiling rejected it at /api/model-check before the first turn could send.
// Pinned explicitly so a future cap change can't silently re-break Opus
// selection.
const opusLatestEntry = {
  slug: "anthropic/claude-opus-4.8",
  name: "Claude Opus 4.8",
  completionPerToken: 0.000025, // $25/M, as OpenRouter reports it
  promptPerToken: 0.000005,
  outputModalities: ["text"],
};
const expensiveEntry = {
  slug: "openai/o1-pro",
  name: "o1 Pro",
  completionPerToken: 0.0006, // $600/M
  promptPerToken: 0.00015,
  outputModalities: ["text"],
};
const imageOnlyEntry = {
  slug: "someone/image-only",
  name: "Image Only",
  completionPerToken: 0.000001,
  promptPerToken: 0.000001,
  outputModalities: ["image"],
};
const imageAndTextEntry = {
  slug: "google/gemini-image",
  name: "Nano-Banana-like",
  completionPerToken: 0.000001,
  promptPerToken: 0.000001,
  outputModalities: ["image", "text"],
};
const textAndAudioEntry = {
  slug: "openai/gpt-audio",
  name: "GPT Audio-like",
  completionPerToken: 0.000001,
  promptPerToken: 0.000001,
  outputModalities: ["text", "audio"],
};
const datedEntry = {
  slug: "moonshotai/kimi-k2.6",
  canonicalSlug: "moonshotai/kimi-k2.6-20260420",
  name: "Kimi K2.6",
  completionPerToken: 0.0000025,
  promptPerToken: 0.0000005,
  outputModalities: ["text"],
};
const legacyNoModalitiesEntry = {
  slug: "legacy/older-model",
  name: "Legacy",
  completionPerToken: 0.000002,
  promptPerToken: 0.000001,
  outputModalities: [],
};

function mockOpenRouter(entries: CatalogEntry[]) {
  globalThis.fetch = vi.fn().mockResolvedValue({
    ok: true,
    json: async () => ({
      data: entries.map((e) => ({
        id: e.slug,
        canonical_slug: e.canonicalSlug,
        name: e.name,
        context_length: e.contextLength,
        created: e.created,
        pricing: {
          completion: e.completionPerToken,
          prompt: e.promptPerToken,
        },
        architecture: {
          output_modalities: e.outputModalities,
        },
      })),
    }),
  }) as unknown as typeof fetch;
}

afterEach(() => {
  __resetCatalogForTests();
  vi.restoreAllMocks();
});

describe("isWithinBudget", () => {
  it("accepts prices up to and including the ceiling", () => {
    expect(isWithinBudget(cheapEntry)).toBe(true);
    expect(isWithinBudget(atCeilingEntry)).toBe(true);
  });

  it("accepts the advanced tier (Opus 4.8, $25/M)", () => {
    expect(isWithinBudget(opusLatestEntry)).toBe(true);
  });

  it("rejects prices above the ceiling", () => {
    expect(isWithinBudget(expensiveEntry)).toBe(false);
  });
});

describe("validateSlug", () => {
  it("rejects an empty slug", async () => {
    mockOpenRouter([cheapEntry]);
    const result = await validateSlug("   ");
    expect(result.ok).toBe(false);
    if (!result.ok) {
      expect(result.reason).toBe("empty");
      expect(result.modelsUrl).toBe(MODELS_PAGE_URL);
    }
  });

  it("accepts an affordable slug from the catalog", async () => {
    mockOpenRouter([cheapEntry]);
    const result = await validateSlug(cheapEntry.slug);
    expect(result.ok).toBe(true);
    if (result.ok) expect(result.entry?.slug).toBe(cheapEntry.slug);
  });

  it("rejects a slug that costs more than the ceiling with a helpful message", async () => {
    mockOpenRouter([expensiveEntry]);
    const result = await validateSlug(expensiveEntry.slug);
    expect(result.ok).toBe(false);
    if (!result.ok) {
      expect(result.reason).toBe("over_budget");
      expect(result.modelsUrl).toBe(MODELS_PAGE_URL);
      expect(result.message).toContain(expensiveEntry.slug);
      expect(result.message).toContain(String(MAX_COMPLETION_USD_PER_MILLION));
      expect(result.message).toContain(MODELS_PAGE_URL);
      expect(result.message).toContain("600.00");
    }
  });

  it("passes through unknown slugs so new models still work if the catalog is stale", async () => {
    mockOpenRouter([cheapEntry]);
    const result = await validateSlug("someone/brand-new-model");
    expect(result.ok).toBe(true);
    if (result.ok) expect(result.entry).toBeUndefined();
  });

  it("trims whitespace before lookup", async () => {
    mockOpenRouter([expensiveEntry]);
    const result = await validateSlug(`  ${expensiveEntry.slug}  `);
    expect(result.ok).toBe(false);
  });
});

describe("parsePrice", () => {
  it("returns the number when given a finite number", () => {
    expect(parsePrice(42)).toBe(42);
    expect(parsePrice(0)).toBe(0);
    expect(parsePrice(-1.5)).toBe(-1.5);
  });

  it("returns NaN when given non-finite numbers", () => {
    expect(parsePrice(Number.POSITIVE_INFINITY)).toBeNaN();
    expect(parsePrice(Number.NEGATIVE_INFINITY)).toBeNaN();
    expect(parsePrice(Number.NaN)).toBeNaN();
  });

  it("parses valid numeric strings", () => {
    expect(parsePrice("42")).toBe(42);
    expect(parsePrice("3.14")).toBe(3.14);
    expect(parsePrice("-0.5")).toBe(-0.5);
    expect(parsePrice("  10  ")).toBe(10);
    expect(parsePrice("0")).toBe(0);
    expect(parsePrice("0.0")).toBe(0);
    expect(parsePrice("1e-5")).toBe(0.00001);
    expect(parsePrice("-1e-5")).toBe(-0.00001);
    expect(parsePrice("0x1A")).toBe(26);
  });

  it("returns NaN for invalid string inputs", () => {
    expect(parsePrice("not a number")).toBeNaN();
    expect(parsePrice("42px")).toBeNaN();
  });

  it("returns NaN for empty or whitespace strings", () => {
    expect(parsePrice("")).toBeNaN();
    expect(parsePrice("   ")).toBeNaN();
  });

  it("returns NaN for edge cases and unexpected types", () => {
    expect(parsePrice(null)).toBeNaN();
    expect(parsePrice(undefined)).toBeNaN();
    expect(parsePrice(true)).toBeNaN();
    expect(parsePrice(false)).toBeNaN();
    expect(parsePrice({})).toBeNaN();
    expect(parsePrice([])).toBeNaN();
    expect(parsePrice([42])).toBeNaN(); // Array with number
  });
});

describe("buildCatalog via loadCatalog", () => {
  it("ignores entries without a numeric completion price", async () => {
    const mockPayload = {
      data: [
        { id: "model-valid", pricing: { completion: "0.0001" } },
        { id: "model-missing-pricing" },
        { id: "model-non-numeric", pricing: { completion: "unknown" } },
        { id: "model-free", pricing: { completion: "0" } },
      ],
    };

    globalThis.fetch = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => mockPayload,
    }) as unknown as typeof fetch;

    const catalog = await loadCatalog();

    expect(catalog.entries.size).toBe(2);
    expect(catalog.entries.has("model-valid")).toBe(true);
    expect(catalog.entries.has("model-free")).toBe(true);
  });
});

describe("__buildCatalogForTests", () => {
  it("ignores entries without a numeric completion price directly", () => {
    const mockPayload = {
      data: [
        { id: "model-valid", pricing: { completion: "0.0001" } },
        { id: "model-missing-pricing" },
        { id: "model-non-numeric", pricing: { completion: "unknown" } },
        { id: "model-free", pricing: { completion: "0" } },
      ],
    };

    const catalog = __buildCatalogForTests(mockPayload);

    expect(catalog.entries.size).toBe(2);
    expect(catalog.entries.has("model-valid")).toBe(true);
    expect(catalog.entries.has("model-free")).toBe(true);
  });

  it("throws an error when response is missing data array", () => {
    expect(() => __buildCatalogForTests({})).toThrow("openrouter models response missing data[] array");
    expect(() => __buildCatalogForTests(null)).toThrow("openrouter models response missing data[] array");
  });

  it("throws an error when response contains no usable entries", () => {
    const mockPayload = {
      data: [
        { id: "model-missing-pricing" },
        { id: "model-non-numeric", pricing: { completion: "unknown" } },
      ],
    };
    expect(() => __buildCatalogForTests(mockPayload)).toThrow("openrouter models response contained no usable entries");
  });
});

describe("isTextCompletion", () => {
  it("accepts models that emit text only", () => {
    expect(isTextCompletion(cheapEntry)).toBe(true);
  });

  it("rejects models that only emit images", () => {
    expect(isTextCompletion(imageOnlyEntry)).toBe(false);
  });

  it("rejects image-generation models that also emit text (Nano-Banana-style)", () => {
    expect(isTextCompletion(imageAndTextEntry)).toBe(false);
  });

  it("rejects audio-generation models that also emit text (GPT-Audio-style)", () => {
    expect(isTextCompletion(textAndAudioEntry)).toBe(false);
  });

  it("passes legacy entries through when modalities are missing", () => {
    expect(isTextCompletion(legacyNoModalitiesEntry)).toBe(true);
  });
});

describe("listAllowed", () => {
  it("includes only within-budget text-only entries, sorted by price desc", async () => {
    mockOpenRouter([
      expensiveEntry,
      imageOnlyEntry,
      imageAndTextEntry,
      textAndAudioEntry,
      cheapEntry,
      atCeilingEntry,
      legacyNoModalitiesEntry,
    ]);
    const catalog = await loadCatalog();
    const slugs = listAllowed(catalog).map((e) => e.slug);
    // At-ceiling ($30/M) first, then legacy ($2/M), then cheap ($0.5/M).
    // o1-pro (over budget), image-only, image+text, and text+audio
    // are all excluded.
    expect(slugs).toEqual([
      atCeilingEntry.slug,
      legacyNoModalitiesEntry.slug,
      cheapEntry.slug,
    ]);
  });
});

describe("resolveSlug", () => {
  it("returns the entry when given the short id", async () => {
    mockOpenRouter([datedEntry]);
    const catalog = await loadCatalog();
    expect(resolveSlug(catalog, datedEntry.slug)?.slug).toBe(datedEntry.slug);
  });

  it("returns the entry when given the dated canonical slug", async () => {
    mockOpenRouter([datedEntry]);
    const catalog = await loadCatalog();
    const hit = resolveSlug(catalog, datedEntry.canonicalSlug!);
    expect(hit?.slug).toBe(datedEntry.slug);
  });

  it("returns undefined for a slug the catalog doesn't know", async () => {
    mockOpenRouter([datedEntry]);
    const catalog = await loadCatalog();
    expect(resolveSlug(catalog, "someone/does-not-exist")).toBeUndefined();
  });
});

describe("validateSlug canonical-slug lookup", () => {
  it("rejects an over-budget dated slug using its short-id price", async () => {
    mockOpenRouter([
      {
        ...expensiveEntry,
        canonicalSlug: `${expensiveEntry.slug}-20260101`,
      },
    ]);
    const result = await validateSlug(`${expensiveEntry.slug}-20260101`);
    expect(result.ok).toBe(false);
  });
});

describe("listLatestPerLab", () => {
  // Two entries per lab so we can assert the function picks the newer
  // one. `created` is a Unix timestamp in seconds — bigger = newer.
  const anthropicOld: CatalogEntry = {
    slug: "anthropic/claude-sonnet-4.6",
    name: "Claude Sonnet 4.6",
    completionPerToken: 0.000015,
    promptPerToken: 0.000003,
    outputModalities: ["text"],
    created: 1_700_000_000,
  };
  const anthropicNew: CatalogEntry = {
    slug: "anthropic/claude-haiku-4.5",
    name: "Claude Haiku 4.5",
    completionPerToken: 0.000004,
    promptPerToken: 0.0000008,
    outputModalities: ["text"],
    created: 1_750_000_000,
  };
  const openaiOld: CatalogEntry = {
    slug: "openai/gpt-5.4",
    name: "GPT 5.4",
    completionPerToken: 0.00001,
    promptPerToken: 0.000002,
    outputModalities: ["text"],
    created: 1_700_000_000,
  };
  const openaiNew: CatalogEntry = {
    slug: "openai/gpt-6",
    name: "GPT 6",
    completionPerToken: 0.000012,
    promptPerToken: 0.0000025,
    outputModalities: ["text"],
    created: 1_760_000_000,
  };
  const xaiEntry: CatalogEntry = {
    slug: "x-ai/grok-5",
    name: "Grok 5",
    completionPerToken: 0.000008,
    promptPerToken: 0.000002,
    outputModalities: ["text"],
    created: 1_740_000_000,
  };
  const overBudgetOpenAI: CatalogEntry = {
    slug: "openai/o2-pro",
    name: "o2 Pro",
    completionPerToken: 0.0006,
    promptPerToken: 0.00015,
    outputModalities: ["text"],
    created: 1_770_000_000,
  };
  const imageGoogle: CatalogEntry = {
    slug: "google/nano-banana-2",
    name: "Nano Banana 2",
    completionPerToken: 0.000001,
    promptPerToken: 0.000001,
    outputModalities: ["image", "text"],
    created: 1_770_000_000,
  };
  const googleText: CatalogEntry = {
    slug: "google/gemini-3-pro",
    name: "Gemini 3 Pro",
    completionPerToken: 0.000007,
    promptPerToken: 0.0000015,
    outputModalities: ["text"],
    created: 1_745_000_000,
  };
  // Cohere shouldn't be picked at all — not in MAJOR_LABS.
  const cohereEntry: CatalogEntry = {
    slug: "cohere/command-x",
    name: "Command X",
    completionPerToken: 0.000005,
    promptPerToken: 0.000001,
    outputModalities: ["text"],
    created: 1_780_000_000,
  };

  it("returns the newest within-budget text-only model per lab", async () => {
    mockOpenRouter([
      anthropicOld,
      anthropicNew,
      openaiOld,
      openaiNew,
      xaiEntry,
      googleText,
    ]);
    const catalog = await loadCatalog();
    const slugs = listLatestPerLab(catalog).map((e) => e.slug);
    expect(slugs).toEqual([
      anthropicNew.slug,
      openaiNew.slug,
      googleText.slug,
      xaiEntry.slug,
    ]);
  });

  it("respects excludeSlugs so tier-pinned models don't duplicate the picker rows", async () => {
    mockOpenRouter([anthropicOld, anthropicNew, openaiNew]);
    const catalog = await loadCatalog();
    const slugs = listLatestPerLab(catalog, [anthropicNew.slug]).map((e) => e.slug);
    // Excluded the newer Anthropic entry; should fall back to the older one.
    expect(slugs).toEqual([anthropicOld.slug, openaiNew.slug]);
  });

  it("skips over-budget and non-text-only models", async () => {
    mockOpenRouter([openaiOld, overBudgetOpenAI, imageGoogle, googleText]);
    const catalog = await loadCatalog();
    const slugs = listLatestPerLab(catalog).map((e) => e.slug);
    // Over-budget GPT excluded, image-output Google excluded.
    expect(slugs).toEqual([openaiOld.slug, googleText.slug]);
  });

  it("omits labs entirely when they have no qualifying entries", async () => {
    mockOpenRouter([openaiOld]);
    const catalog = await loadCatalog();
    const slugs = listLatestPerLab(catalog).map((e) => e.slug);
    expect(slugs).toEqual([openaiOld.slug]);
  });

  it("ignores labs not in MAJOR_LABS (Cohere, Meta)", async () => {
    mockOpenRouter([cohereEntry, openaiOld]);
    const catalog = await loadCatalog();
    const slugs = listLatestPerLab(catalog).map((e) => e.slug);
    expect(slugs).toEqual([openaiOld.slug]);
    expect(slugs).not.toContain(cohereEntry.slug);
  });

  it("skips :free variants even when they are the newest", async () => {
    const nvFree: CatalogEntry = {
      slug: "nvidia/nemotron-3:free",
      name: "Nemotron 3 (free)",
      completionPerToken: 0,
      promptPerToken: 0,
      outputModalities: ["text"],
      created: 1_790_000_000,
    };
    const nvPaid: CatalogEntry = {
      slug: "nvidia/nemotron-3",
      name: "Nemotron 3",
      completionPerToken: 0.000003,
      promptPerToken: 0.0000005,
      outputModalities: ["text"],
      created: 1_780_000_000,
    };
    mockOpenRouter([nvFree, nvPaid]);
    const catalog = await loadCatalog();
    const slugs = listLatestPerLab(catalog).map((e) => e.slug);
    expect(slugs).toEqual([nvPaid.slug]);
  });

  it("breaks ties on `created` by preferring the higher completion price (flagship proxy)", async () => {
    const sameDayCheap: CatalogEntry = {
      slug: "openai/o-mini-tied",
      name: "Cheap Tied",
      completionPerToken: 0.000003,
      promptPerToken: 0.000001,
      outputModalities: ["text"],
      created: 1_780_000_000,
    };
    const sameDayExpensive: CatalogEntry = {
      slug: "openai/o-pro-tied",
      name: "Expensive Tied",
      completionPerToken: 0.000012,
      promptPerToken: 0.0000025,
      outputModalities: ["text"],
      created: 1_780_000_000,
    };
    mockOpenRouter([sameDayCheap, sameDayExpensive]);
    const catalog = await loadCatalog();
    const slugs = listLatestPerLab(catalog).map((e) => e.slug);
    expect(slugs).toEqual([sameDayExpensive.slug]);
  });
});
