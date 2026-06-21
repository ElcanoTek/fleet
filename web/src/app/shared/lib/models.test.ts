import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import {
  filterModels,
  isAffordable,
  isTextOutput,
  loadModels,
  normaliseModel,
  scoreMatch,
  SEED_MODELS,
  _resetModelCacheForTests,
  type PickerModel,
} from "./models";

// Pure-logic tests for the ModelPicker's catalog core (ported from moc's
// model-picker.js). The DOM/positioning is tested via the ModelPicker.test.tsx
// component test; here we cover affordability, modality, ranking, and the
// fetch-fallback-to-seeds behavior.

describe("isAffordable", () => {
  it("keeps models under the per-token ceilings", () => {
    expect(isAffordable({ pricing: { prompt: "0.000003", completion: "0.000015" } })).toBe(true);
  });
  it("drops models over the ceilings or with missing pricing", () => {
    expect(isAffordable({ pricing: { prompt: "0.00001", completion: "0.00005" } })).toBe(false);
    expect(isAffordable({ pricing: {} })).toBe(false);
  });
});

describe("isTextOutput", () => {
  it("keeps text and vision (text-in/out) models", () => {
    expect(isTextOutput({ architecture: { output_modalities: ["text"] } })).toBe(true);
    expect(isTextOutput({ architecture: { modality: "text+image->text" } })).toBe(true);
  });
  it("drops image-output / unknown models", () => {
    expect(isTextOutput({ architecture: { modality: "text->image" } })).toBe(false);
    expect(isTextOutput({})).toBe(false);
  });
});

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

describe("normaliseModel", () => {
  it("returns null for an id-less raw model", () => {
    expect(normaliseModel({})).toBeNull();
  });
  it("captures created + completion price for tie-breaking", () => {
    const m = normaliseModel({ id: "a/b", created: 123, pricing: { completion: "0.000005" } });
    expect(m).toMatchObject({ id: "a/b", created: 123, priceCompletion: 0.000005 });
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

  it("merges seeds with the fetched catalog", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: true,
        json: async () => ({
          data: [
            {
              id: "deepseek/deepseek-v3.2",
              name: "DeepSeek V3.2",
              pricing: { prompt: "0.000001", completion: "0.000002" },
              architecture: { output_modalities: ["text"] },
            },
          ],
        }),
      }),
    );
    const models = await loadModels();
    expect(models.some((m) => m.id === "anthropic/claude-opus-4.8")).toBe(true);
    expect(models.some((m) => m.id === "deepseek/deepseek-v3.2")).toBe(true);
  });

  it("falls back to the seed list when the fetch fails", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new Error("network down")));
    const models = await loadModels();
    expect(models).toEqual(SEED_MODELS);
  });
});
