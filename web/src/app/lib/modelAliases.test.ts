import { describe, expect, it } from "vitest";

import {
  ADVANCED_MODEL,
  DEFAULT_MODEL,
  TIER_MODELS,
  labelForModel,
  tierForModel,
} from "./modelAliases";

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
  it("returns the friendly alias for tier slots", () => {
    expect(labelForModel(DEFAULT_MODEL)).toBe("default");
    expect(labelForModel(ADVANCED_MODEL)).toBe("advanced");
  });

  it("returns the raw slug for non-tier models", () => {
    expect(labelForModel("openai/gpt-5.4")).toBe("openai/gpt-5.4");
    expect(labelForModel("meta-llama/llama-3.3-70b-instruct")).toBe(
      "meta-llama/llama-3.3-70b-instruct",
    );
  });
});

describe("TIER_MODELS", () => {
  it("lists default, advanced in that exact order", () => {
    // The picker pins this order at the top of the dropdown; the
    // sequence is product-meaningful (cheap everyday pick → strongest).
    expect(TIER_MODELS.map((t) => t.label)).toEqual(["default", "advanced"]);
    expect(TIER_MODELS.map((t) => t.slug)).toEqual([DEFAULT_MODEL, ADVANCED_MODEL]);
  });
});
