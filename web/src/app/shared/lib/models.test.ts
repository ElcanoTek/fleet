import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  compactModelLabel,
  filterModels,
  loadModels,
  normaliseCatalogModel,
  scoreMatch,
  seedModels,
  _resetModelCacheForTests,
  type PickerModel,
} from "./models";

// Pure-logic tests for the ModelPicker's catalog core (ported from moc's
// model-picker.js). The DOM/positioning is tested via the ModelPicker.test.tsx
// component test; here we cover ranking, normalisation, and the
// fetch-fallback-to-seeds behavior. The catalog arrives via the same-origin
// /api/model-catalog proxy (the CSP blocks a direct openrouter.ai fetch), so
// the mocks speak that endpoint's {models: [...]} shape.

describe("scoreMatch / filterModels", () => {
  const models: PickerModel[] = [
    { id: "anthropic/claude-opus-4.8", name: "Anthropic: Claude Opus 4.8", recommended: true },
    { id: "google/gemini-3.5-flash", name: "Google: Gemini 3.5 Flash", recommended: true },
    { id: "deepseek/deepseek-v3.2", name: "DeepSeek: V3.2", recommended: false },
  ];

  it("scores exact > prefix > substring", () => {
    expect(scoreMatch(models[0], "anthropic/claude-opus-4.8")).toBe(1000);
    expect(scoreMatch(models[0], "anthropic")).toBe(500);
    expect(scoreMatch(models[0], "opus")).toBe(200);
  });

  it("returns the natural order capped when query is empty", () => {
    expect(filterModels(models, "").map((m) => m.id)).toEqual(models.map((m) => m.id));
  });

  it("ranks matches by score for a non-empty query", () => {
    const out = filterModels(models, "gemini");
    expect(out[0].id).toBe("google/gemini-3.5-flash");
  });

  it("returns nothing for a query that matches no model", () => {
    expect(filterModels(models, "zzz-nonexistent")).toEqual([]);
  });
});

describe("compactModelLabel", () => {
  it("drops the vendor prefix", () => {
    expect(compactModelLabel("Z.AI: GLM 5.2")).toBe("GLM 5.2");
    expect(compactModelLabel("OpenAI: GPT-5.6 Sol")).toBe("GPT-5.6 Sol");
  });

  it("keeps everything after the first separator", () => {
    expect(compactModelLabel("Anthropic: Claude: Opus")).toBe("Claude: Opus");
  });

  it("leaves labels without a vendor prefix alone", () => {
    expect(compactModelLabel("default")).toBe("default");
    expect(compactModelLabel("z-ai/glm-5.2")).toBe("z-ai/glm-5.2");
    // A colon with no following space is part of the name, not a prefix.
    expect(compactModelLabel("weird:name")).toBe("weird:name");
  });

  it("falls back to the full label when nothing would be left", () => {
    expect(compactModelLabel("Vendor:   ")).toBe("Vendor:   ");
    expect(compactModelLabel("")).toBe("");
  });
});

describe("normaliseCatalogModel", () => {
  it("returns null for a slug-less raw model", () => {
    expect(normaliseCatalogModel({})).toBeNull();
  });
  it("captures created + completion price for tie-breaking", () => {
    const m = normaliseCatalogModel({
      slug: "a/b",
      created: 123,
      price_completion: 0.000005,
      context_length: 200000,
    });
    expect(m).toMatchObject({
      id: "a/b",
      created: 123,
      priceCompletion: 0.000005,
      contextLength: 200000,
    });
  });
});

describe("loadModels (fetch + fallback)", () => {
  beforeEach(() => {
    _resetModelCacheForTests();
  });
  afterEach(() => {
    vi.restoreAllMocks();
    _resetModelCacheForTests();
  });

  it("merges seeds with the proxied catalog", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation(async (url: string) => {
        if (String(url).includes("/api/model-catalog")) {
          return {
            ok: true,
            json: async () => ({
              models: [
                {
                  slug: "deepseek/deepseek-v3.2",
                  name: "DeepSeek V3.2",
                  price_completion: 0.000002,
                },
              ],
            }),
          };
        }
        return { ok: true, json: async () => ({ models: [], providers: [] }) };
      }),
    );
    const models = await loadModels();
    expect(models.some((m) => m.id === "google/gemini-3.8-flash")).toBe(true);
    expect(models.some((m) => m.id === "deepseek/deepseek-v3.2")).toBe(true);
  });

  it("joins catalog prices onto the hand-written seed entries", async () => {
    // dedupeAndOrder keeps the seed row and drops the fetched duplicate, so
    // without the enrichment pass the two pinned slugs would be the only rows
    // in the picker with no cost indicator.
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation(async (url: string) => {
        if (String(url).includes("/api/model-catalog")) {
          return {
            ok: true,
            json: async () => ({
              models: [
                {
                  slug: "google/gemini-3.8-flash",
                  name: "Google: Gemini 3.8 Flash",
                  price_prompt: 0.0000004,
                  price_completion: 0.0000016,
                  context_length: 200000,
                },
              ],
            }),
          };
        }
        return { ok: true, json: async () => ({ models: [], providers: [] }) };
      }),
    );
    const models = await loadModels();
    const seeded = models.find((m) => m.id === "google/gemini-3.8-flash");
    expect(seeded).toMatchObject({
      // Seed facts win: the curated name and the recommended flag survive.
      name: "Google: Gemini 3.8 Flash",
      recommended: true,
      pricePrompt: 0.0000004,
      priceCompletion: 0.0000016,
      contextLength: 200000,
    });
    // And the row is not duplicated by the catalog entry.
    expect(models.filter((m) => m.id === "google/gemini-3.8-flash")).toHaveLength(1);
  });

  it("falls back to the seed list when the fetch fails", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    const models = await loadModels();
    expect(models).toEqual(seedModels());
  });

  it("puts workspace-provider models first, flagged for the badge", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation(async (url: string) => {
        if (String(url).includes("/api/llm-provider-models")) {
          return {
            ok: true,
            json: async () => ({
              models: [{ id: "anthropic-direct/claude-opus-4-8", name: "anthropic-direct: claude-opus-4-8" }],
              providers: [{ name: "anthropic-direct", type: "anthropic", catch_all: false }],
            }),
          };
        }
        return { ok: true, json: async () => ({ models: [] }) };
      }),
    );
    const models = await loadModels();
    expect(models[0]).toMatchObject({
      id: "anthropic-direct/claude-opus-4-8",
      workspace: true,
    });
    // The catalog/seed entries still follow.
    expect(models.some((m) => m.id === "google/gemini-3.8-flash")).toBe(true);
  });

  it("expands a catch-all workspace provider from the catwalk catalog", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation(async (url: string) => {
        if (String(url).includes("/api/llm-provider-models")) {
          return {
            ok: true,
            json: async () => ({
              models: [],
              providers: [{ name: "my-anthropic", type: "anthropic", catch_all: true }],
            }),
          };
        }
        if (String(url).includes("/api/catwalk-models")) {
          return {
            ok: true,
            json: async () => ({
              providers: [
                {
                  id: "anthropic",
                  name: "Anthropic",
                  type: "anthropic",
                  models: [
                    { id: "claude-opus-4-8", name: "Claude Opus 4.8", context_window: 1000000 },
                  ],
                },
              ],
            }),
          };
        }
        return { ok: true, json: async () => ({ models: [] }) };
      }),
    );
    const models = await loadModels();
    expect(models[0]).toMatchObject({
      id: "my-anthropic/claude-opus-4-8",
      name: "my-anthropic: Claude Opus 4.8",
      workspace: true,
      contextLength: 1000000,
    });
  });

  it("ignores a failing workspace-models endpoint", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation(async (url: string) => {
        if (String(url).includes("/api/llm-provider-models")) {
          return { ok: false, json: async () => ({}) };
        }
        return { ok: true, json: async () => ({ models: [] }) };
      }),
    );
    const models = await loadModels();
    expect(models).toEqual(seedModels());
  });
});
