import { describe, expect, it } from "vitest";
import {
  blendedPricePerMillion,
  formatBlendedPrice,
  modelCostFor,
  tierForBlendedPrice,
} from "./modelCost";

// Unit tests for the restaurant-style cost tiers shown in both model pickers.
// The numbers below are per-TOKEN prices in OpenRouter's format (USD/token),
// so 0.000003 is "$3 per 1M tokens".

describe("blendedPricePerMillion", () => {
  it("weights prompt 3:1 against completion", () => {
    // $3/M prompt, $15/M completion → (3*3 + 15) / 4 = $6/M.
    expect(
      blendedPricePerMillion({ pricePrompt: 0.000003, priceCompletion: 0.000015 }),
    ).toBeCloseTo(6, 6);
  });

  it("uses whichever side is known when the other is missing", () => {
    expect(blendedPricePerMillion({ priceCompletion: 0.000002 })).toBeCloseTo(2, 6);
    expect(blendedPricePerMillion({ pricePrompt: 0.0000005 })).toBeCloseTo(0.5, 6);
  });

  it("returns null when neither side is a usable number", () => {
    expect(blendedPricePerMillion({})).toBeNull();
    expect(blendedPricePerMillion({ pricePrompt: null, priceCompletion: undefined })).toBeNull();
    // OpenRouter occasionally emits "-1"/junk for unpriced entries; a negative
    // or non-numeric price is not a tier, it's an unknown.
    expect(blendedPricePerMillion({ pricePrompt: -1 })).toBeNull();
    expect(
      blendedPricePerMillion({ pricePrompt: Number.NaN, priceCompletion: Number.NaN }),
    ).toBeNull();
  });

  it("treats a free model as a real $0 price, not unknown", () => {
    expect(blendedPricePerMillion({ pricePrompt: 0, priceCompletion: 0 })).toBe(0);
  });
});

describe("tierForBlendedPrice", () => {
  it("buckets by the documented per-1M thresholds", () => {
    expect(tierForBlendedPrice(0)).toBe(1);
    expect(tierForBlendedPrice(1)).toBe(1);
    expect(tierForBlendedPrice(1.01)).toBe(2);
    expect(tierForBlendedPrice(5)).toBe(2);
    expect(tierForBlendedPrice(6)).toBe(3);
    expect(tierForBlendedPrice(15)).toBe(3);
    expect(tierForBlendedPrice(15.01)).toBe(4);
    expect(tierForBlendedPrice(30)).toBe(4);
  });
});

describe("formatBlendedPrice", () => {
  it("renders sub-dollar prices with three decimals", () => {
    expect(formatBlendedPrice(0.075)).toBe("$0.075/M tokens");
  });
  it("renders dollar-plus prices with two decimals", () => {
    expect(formatBlendedPrice(6)).toBe("$6.00/M tokens");
  });
  it("names free models rather than printing $0.000", () => {
    expect(formatBlendedPrice(0)).toBe("free");
  });
});

describe("modelCostFor", () => {
  it("maps a cheap model to one dollar sign", () => {
    const cost = modelCostFor({ pricePrompt: 0.0000003, priceCompletion: 0.0000012 });
    expect(cost?.tier).toBe(1);
    expect(cost?.symbol).toBe("$");
    expect(cost?.label).toBe("budget");
    expect(cost?.description).toContain("$ of $$$$");
  });

  it("maps a frontier model to three dollar signs", () => {
    const cost = modelCostFor({ pricePrompt: 0.000003, priceCompletion: 0.000015 });
    expect(cost?.tier).toBe(3);
    expect(cost?.symbol).toBe("$$$");
    expect(cost?.description).toContain("$6.00/M tokens");
  });

  it("maps a flagship model to four dollar signs", () => {
    const cost = modelCostFor({ pricePrompt: 0.000015, priceCompletion: 0.000075 });
    expect(cost?.tier).toBe(4);
    expect(cost?.symbol).toBe("$$$$");
    expect(cost?.label).toBe("top tier");
  });

  it("describes a free model as free", () => {
    expect(modelCostFor({ pricePrompt: 0, priceCompletion: 0 })?.description).toContain("Free");
  });

  it("returns null for models with no known pricing", () => {
    // Workspace-provider models and half-typed custom slugs land here — the
    // UI must render nothing rather than implying a tier.
    expect(modelCostFor({})).toBeNull();
    expect(modelCostFor(null)).toBeNull();
    expect(modelCostFor(undefined)).toBeNull();
  });
});
