import { describe, expect, it } from "vitest";
import { formatCreditAmount, formatCredits } from "./credits";

// Money is shown as credits, never with a currency symbol or code.
describe("formatCredits", () => {
  it("renders two decimals with the unit", () => {
    expect(formatCredits(4.5)).toBe("4.50 credits");
    expect(formatCredits(0)).toBe("0.00 credits");
    expect(formatCredits(1234.5)).toBe("1,234.50 credits");
  });

  it("widens a non-zero amount under 0.01 so it does not read as zero", () => {
    expect(formatCredits(0.004)).toBe("0.004 credits");
    expect(formatCredits(0.00012)).toBe("0.0001 credits");
  });

  it("honours a fixed precision", () => {
    expect(formatCredits(0.36, 4)).toBe("0.3600 credits");
    expect(formatCredits(8.286, 2)).toBe("8.29 credits");
  });

  it("treats a missing or non-finite amount as zero", () => {
    expect(formatCredits(null)).toBe("0.00 credits");
    expect(formatCredits(undefined)).toBe("0.00 credits");
    expect(formatCredits(Number.NaN)).toBe("0.00 credits");
  });

  it("never prints a currency symbol or code", () => {
    for (const v of [0, 0.004, 4.5, 1234.5]) {
      expect(formatCredits(v)).not.toMatch(/\$|USD/);
      expect(formatCreditAmount(v)).not.toMatch(/\$|USD|credit/);
    }
  });
});
