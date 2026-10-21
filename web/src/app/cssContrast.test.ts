import { describe, expect, it } from "vitest";

import { resolveColor } from "./cssContrast";

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

  it("reads #rgba with its alpha", () => {
    expect(resolveColor("#0008", {})[3]).toBeCloseTo(0x88 / 255);
  });
});
