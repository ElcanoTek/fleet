import { describe, expect, it } from "vitest";

import { contrastRatio, resolveColor } from "./cssContrast";

// The evaluator must fail loudly on anything the browser would reject, or a
// typo'd token passes the contrast suites while the page falls back to some
// other color.
describe("resolveColor", () => {
  it.each(["#abc", "#abcd", "#aabbcc", "#aabbccdd"])("reads the legal hex length %s", (hex) => {
    expect(() => resolveColor(hex, {})).not.toThrow();
  });

  it.each(["#ab", "#abcde", "#aabbccd", "#aabbccddee"])("rejects the illegal hex length %s", (hex) => {
    expect(() => resolveColor(hex, {})).toThrow(/unsupported color value/);
  });

  it.each([
    ["rgb(255, 0, 0)", [1, 0, 0, 1]],
    ["rgba(0, 0, 61, 0.05)", [0, 0, 61 / 255, 0.05]],
    ["rgb(100% 0% 0%)", [1, 0, 0, 1]],
    ["rgb(0 0 0 / 50%)", [0, 0, 0, 0.5]],
  ] as const)("reads %s", (value, want) => {
    const got = resolveColor(value, {});
    want.forEach((w, i) => expect(got[i]).toBeCloseTo(w));
  });

  it.each([
    "rgb(255, 0)",
    "rgb(1, 2, 3, 4, 5)",
    "rgb(red, 0, 0)",
    "rgb(1e, 0, 0)",
    "rgba(0, 0, 0, NaN)",
    // Mixed or missing separators, which the browser rejects.
    "rgb(0, 0, 0 / 50%)",
    "rgb(0 0, 0)",
    "rgb(0 0 0 0)",
    "rgb(0 0 0 / 50% / 1)",
    "rgb(0 0 0 /)",
    "rgb(0, 0 0)",
  ])(
    "rejects the malformed %s",
    (value) => {
      expect(() => resolveColor(value, {})).toThrow(/unsupported color value/);
    },
  );

  it.each([
    "color-mix(in srgb, #fff 120%, #000)",
    "color-mix(in srgb, #fff 0%, #000 0%)",
    "color-mix(in srgb, #fff)",
    "color-mix(in srgb, #fff, #000, #888)",
    "color-mix(in oklch, #fff, #000)",
  ])("rejects the color-mix the browser would reject or this evaluator cannot read: %s", (value) => {
    expect(() => resolveColor(value, {})).toThrow();
  });

  it("mixes in srgb with CSS Color 5's alpha rule", () => {
    expect(resolveColor("color-mix(in srgb, #ffffff 50%, #000000)", {}).slice(0, 3)).toEqual([0.5, 0.5, 0.5]);
    // Percentages summing under 100% scale the result's alpha.
    expect(resolveColor("color-mix(in srgb, #000 20%, transparent 30%)", {})[3]).toBeCloseTo(0.2);
  });

  it("refuses to measure a non-finite color", () => {
    expect(() => contrastRatio([Number.NaN, 0, 0], [1, 1, 1])).toThrow(/non-finite/);
  });

  it("reads #rgba with its alpha", () => {
    expect(resolveColor("#0008", {})[3]).toBeCloseTo(0x88 / 255);
  });
});
