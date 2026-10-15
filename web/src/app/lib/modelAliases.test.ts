import { afterEach, describe, expect, it } from "vitest";

import {
  ADVANCED_MODEL,
  ADVANCED_MODEL_LABEL,
  DEFAULT_MODEL,
  DEFAULT_MODEL_LABEL,
  DEFAULT_TASK_FALLBACK_MODEL,
  TIER_MODELS,
  _resetModelTiersForTests,
  currentAdvancedModel,
  currentDefaultModel,
  currentTaskFallbackModel,
  currentTierModels,
  labelForModel,
  setModelTiers,
  currentPinnedModels,
  pinnedPrices,
  pinnedModelFor,
  tierForModel,
} from "./modelAliases";

afterEach(() => _resetModelTiersForTests());

describe("tierForModel", () => {
  it("returns the matching tier name for each tier slug", () => {
    expect(tierForModel(DEFAULT_MODEL)).toBe("default");
    expect(tierForModel(ADVANCED_MODEL)).toBe("advanced");
  });

  it("flags known-good slugs we have validated as `tested`", () => {
    expect(tierForModel("openai/gpt-5.4")).toBe("tested");
  });

  it("treats anything else as `experimental`", () => {
    // A slug nobody has vetted, but should still work via OpenRouter.
    expect(tierForModel("meta-llama/llama-3.3-70b-instruct")).toBe("experimental");
    // Empty slug is the "use server default" sentinel — the picker
    // never asks for its tier, but the function should still be safe.
    expect(tierForModel("")).toBe("experimental");
  });
});

describe("labelForModel", () => {
  it("returns the display name for pinned slots (never an alias)", () => {
    expect(labelForModel(DEFAULT_MODEL)).toBe(DEFAULT_MODEL_LABEL);
    expect(labelForModel(ADVANCED_MODEL)).toBe(ADVANCED_MODEL_LABEL);
  });

  it("returns the raw slug for non-tier models", () => {
    expect(labelForModel("openai/gpt-5.4")).toBe("openai/gpt-5.4");
    expect(labelForModel("meta-llama/llama-3.3-70b-instruct")).toBe(
      "meta-llama/llama-3.3-70b-instruct",
    );
  });
});

describe("TIER_MODELS", () => {
  it("pins the recommended pick first, the strong tier second", () => {
    // The picker pins this order at the top of the dropdown; the
    // sequence is product-meaningful (everyday pick → strongest).
    expect(TIER_MODELS.map((t) => t.label)).toEqual([DEFAULT_MODEL_LABEL, ADVANCED_MODEL_LABEL]);
    expect(TIER_MODELS.map((t) => t.slug)).toEqual([DEFAULT_MODEL, ADVANCED_MODEL]);
  });
});

// The live tier pair (#1187): admin-configured slugs arrive with
// /api/client-config and must reroute every classification, label, and
// pinned row — while missing/empty fields keep the compiled-in fallback.
describe("setModelTiers", () => {
  it("defaults to the compiled-in pair before any config lands", () => {
    expect(currentDefaultModel()).toBe(DEFAULT_MODEL);
    expect(currentAdvancedModel()).toBe(ADVANCED_MODEL);
    expect(currentTierModels().map((t) => t.slug)).toEqual([DEFAULT_MODEL, ADVANCED_MODEL]);
  });

  it("installs admin-configured slugs across getters, tiers, and pinned rows", () => {
    setModelTiers({ default_model: "acme/frontier-1", advanced_model: "myBedrock/claude-opus-5" });
    expect(currentDefaultModel()).toBe("acme/frontier-1");
    expect(currentAdvancedModel()).toBe("myBedrock/claude-opus-5");
    expect(tierForModel("acme/frontier-1")).toBe("default");
    expect(tierForModel("myBedrock/claude-opus-5")).toBe("advanced");
    // The former defaults lose their pinned badge but keep working.
    expect(tierForModel(DEFAULT_MODEL)).toBe("experimental");
    // An unknown slug labels as itself — honest, no fabricated name.
    expect(currentTierModels()).toEqual([
      { slug: "acme/frontier-1", label: "acme/frontier-1" },
      { slug: "myBedrock/claude-opus-5", label: "myBedrock/claude-opus-5" },
    ]);
    // A tier that IS a compiled-in slug keeps its friendly label.
    setModelTiers({ default_model: ADVANCED_MODEL, advanced_model: "acme/frontier-1" });
    expect(currentTierModels()[0]).toEqual({ slug: ADVANCED_MODEL, label: ADVANCED_MODEL_LABEL });
  });

  it("carries the operator's scheduler fallback for the task form, else the compiled-in one", () => {
    expect(currentTaskFallbackModel()).toBe(DEFAULT_TASK_FALLBACK_MODEL);
    setModelTiers({ default_model: "acme/frontier-1", task_fallback_model: "acme/cheap-1" });
    expect(currentTaskFallbackModel()).toBe("acme/cheap-1");
    // The task fallback is its own slot: it never shadows the chat tiers.
    expect(currentAdvancedModel()).toBe(ADVANCED_MODEL);
    setModelTiers({ task_fallback_model: "  " });
    expect(currentTaskFallbackModel()).toBe(DEFAULT_TASK_FALLBACK_MODEL);
  });

  it("keeps the fallback for missing, empty, or whitespace fields", () => {
    setModelTiers({ default_model: "acme/frontier-1" });
    expect(currentAdvancedModel()).toBe(ADVANCED_MODEL);
    setModelTiers({ default_model: "  ", advanced_model: "" });
    expect(currentDefaultModel()).toBe(DEFAULT_MODEL);
    expect(currentAdvancedModel()).toBe(ADVANCED_MODEL);
    setModelTiers(null);
    expect(currentDefaultModel()).toBe(DEFAULT_MODEL);
  });
});

describe("pinned models (FLEET_PINNED_MODELS)", () => {
  it("installs well-formed pins in order and relabels them", () => {
    setModelTiers({
      pinned: [
        { slug: " acme/open-70b ", label: "Elcano Nova", tag: "alpha" },
        { slug: "acme/open-8b" },
        { slug: "acme/open-70b", label: "duplicate dropped" },
        { label: "no slug dropped" },
        "garbage",
      ],
    });
    expect(currentPinnedModels()).toEqual([
      { slug: "acme/open-70b", label: "Elcano Nova", tag: "alpha" },
      { slug: "acme/open-8b" },
    ]);
    expect(labelForModel("acme/open-70b")).toBe("Elcano Nova");
    expect(labelForModel("acme/open-8b")).toBe("acme/open-8b");
    expect(pinnedModelFor("acme/open-70b")?.tag).toBe("alpha");
  });

  it("degrades a malformed or missing pin list to no pins", () => {
    setModelTiers({ pinned: { slug: "a/b" } });
    expect(currentPinnedModels()).toEqual([]);
    setModelTiers({ pinned: [{ slug: "a/b" }] });
    setModelTiers({});
    expect(currentPinnedModels()).toEqual([]);
  });
});

describe("pinned prices", () => {
  it("parses per-million overrides and free, dropping bad values", () => {
    setModelTiers({
      pinned: [
        { slug: "a/priced", input_price_per_mtok: 1.5, output_price_per_mtok: 0 },
        { slug: "a/free", free: true },
        { slug: "a/bad", input_price_per_mtok: -1, output_price_per_mtok: "2" },
      ],
    });
    const [priced, free, bad] = currentPinnedModels();
    expect(pinnedPrices(priced)).toEqual({ pricePrompt: 1.5e-6, priceCompletion: 0 });
    expect(pinnedPrices(free)).toEqual({ pricePrompt: 0, priceCompletion: 0, free: true });
    expect(pinnedPrices(bad)).toEqual({});
    expect(pinnedPrices(undefined)).toEqual({});
  });
});
