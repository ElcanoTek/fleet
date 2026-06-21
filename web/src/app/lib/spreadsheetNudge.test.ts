import { describe, expect, it } from "vitest";
import { ADVANCED_MODEL, DEFAULT_MODEL } from "./modelAliases";
import {
  HEAVY_SPREADSHEET_THRESHOLD_BYTES,
  decideSpreadsheetNudge,
  isHeavySpreadsheet,
} from "./spreadsheetNudge";

const heavyXlsx = { name: "Tracking Q2.xlsx", size: HEAVY_SPREADSHEET_THRESHOLD_BYTES };
const tinyXlsx = { name: "summary.xlsx", size: 10_000 };
const heavyPdf = { name: "report.pdf", size: 2_000_000 };

describe("isHeavySpreadsheet", () => {
  it("flags an .xlsx at or above the threshold", () => {
    expect(isHeavySpreadsheet(heavyXlsx)).toBe(true);
  });

  it("ignores small .xlsx files (single-sheet ad-hoc exports)", () => {
    expect(isHeavySpreadsheet(tinyXlsx)).toBe(false);
  });

  it("ignores non-spreadsheet attachments regardless of size", () => {
    expect(isHeavySpreadsheet(heavyPdf)).toBe(false);
  });

  it("matches case-insensitively on the extension", () => {
    expect(isHeavySpreadsheet({ name: "TRACKING.XLSX", size: 1_000_000 })).toBe(true);
  });

  it("matches macro-enabled .xlsm and legacy .xls", () => {
    expect(isHeavySpreadsheet({ name: "macro.xlsm", size: 1_000_000 })).toBe(true);
    expect(isHeavySpreadsheet({ name: "legacy.xls", size: 1_000_000 })).toBe(true);
  });
});

describe("decideSpreadsheetNudge", () => {
  const baseArgs = {
    attachments: [heavyXlsx],
    selectedModel: DEFAULT_MODEL,
    dismissed: false,
  };

  it("fires when the default model is selected and a heavy xlsx is attached", () => {
    const decision = decideSpreadsheetNudge(baseArgs);
    expect(decision.show).toBe(true);
    expect(decision.recommendedModel).toBe(ADVANCED_MODEL);
  });

  it("stays quiet when the user has already switched off the default model", () => {
    const decision = decideSpreadsheetNudge({ ...baseArgs, selectedModel: ADVANCED_MODEL });
    expect(decision.show).toBe(false);
  });

  it("stays quiet when the user has explicitly typed a third-party slug", () => {
    const decision = decideSpreadsheetNudge({ ...baseArgs, selectedModel: "openai/gpt-5" });
    expect(decision.show).toBe(false);
  });

  it("stays quiet when there are no attachments", () => {
    expect(decideSpreadsheetNudge({ ...baseArgs, attachments: [] }).show).toBe(false);
  });

  it("stays quiet for tiny spreadsheets", () => {
    expect(decideSpreadsheetNudge({ ...baseArgs, attachments: [tinyXlsx] }).show).toBe(false);
  });

  it("stays quiet for non-spreadsheet attachments", () => {
    expect(decideSpreadsheetNudge({ ...baseArgs, attachments: [heavyPdf] }).show).toBe(false);
  });

  it("respects an explicit dismiss", () => {
    expect(decideSpreadsheetNudge({ ...baseArgs, dismissed: true }).show).toBe(false);
  });

  it("fires when at least one attachment in a mixed batch qualifies", () => {
    const mixed = { ...baseArgs, attachments: [heavyPdf, tinyXlsx, heavyXlsx] };
    expect(decideSpreadsheetNudge(mixed).show).toBe(true);
  });

  it("honors injected slug overrides (test isolation from production constants)", () => {
    const decision = decideSpreadsheetNudge({
      ...baseArgs,
      selectedModel: "fast/test-tier",
      defaultModel: "fast/test-tier",
      advancedModel: "smart/test-tier",
    });
    expect(decision.show).toBe(true);
    expect(decision.recommendedModel).toBe("smart/test-tier");
  });
});
