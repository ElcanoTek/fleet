import { describe, expect, it } from "vitest";
import { LABEL_COLORS, labelColor, labelChipStyle } from "./labelColors";

describe("labelColor", () => {
  it("is deterministic for the same name", () => {
    expect(labelColor("client")).toBe(labelColor("client"));
    expect(labelColor("urgent")).toBe(labelColor("urgent"));
  });

  it("always returns a color from the palette", () => {
    for (const name of ["client", "urgent", "gpt-review", "reconciliation", "", "a", "zzzzzzz"]) {
      expect(LABEL_COLORS).toContain(labelColor(name));
    }
  });

  it("distributes distinct names across more than one color", () => {
    const names = Array.from({ length: 40 }, (_, i) => `label-${i}`);
    const distinct = new Set(names.map(labelColor));
    expect(distinct.size).toBeGreaterThan(1);
  });

  it("falls back to the first color for an empty name", () => {
    expect(labelColor("")).toBe(LABEL_COLORS[0]);
  });
});

describe("labelChipStyle", () => {
  it("exposes the color via the --chip custom property", () => {
    const style = labelChipStyle("client") as Record<string, string>;
    expect(style["--chip"]).toBe(labelColor("client"));
  });
});
