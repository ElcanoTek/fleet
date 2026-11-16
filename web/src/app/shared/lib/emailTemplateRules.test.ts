import { describe, expect, it } from "vitest";
import { blockLabel, limitPhrases, neverSayLabel } from "./emailTemplateRules";

describe("limitPhrases", () => {
  it("words a bullets-only section", () => {
    expect(limitPhrases({ max_paragraphs: 0, min_bullets: 1, max_bullets: 2, max_words_per_bullet: 35 })).toEqual([
      "No paragraphs",
      "1 to 2 bullets",
      "35 words per bullet at most",
    ]);
  });

  it("words a prose section and a table", () => {
    expect(limitPhrases({ max_paragraphs: 1, max_bullets: 0, max_words: 60 })).toEqual([
      "Up to 60 words",
      "One paragraph",
      "No bullets",
    ]);
    expect(limitPhrases({ max_rows: 4, max_columns: 6 })).toEqual(["Up to 4 rows", "Up to 6 columns"]);
  });

  it("says exactly when the minimum and maximum agree", () => {
    expect(limitPhrases({ min_items: 4, max_items: 4 })).toEqual(["Exactly 4 numbers"]);
    expect(limitPhrases({ min_items: 3, max_items: 4 })).toEqual(["3 to 4 numbers"]);
    expect(limitPhrases({ min_bullets: 5, max_bullets: 5 })).toEqual(["Exactly 5 bullets"]);
  });

  it("leaves out a limit it does not know, and copes with none", () => {
    expect(limitPhrases({ max_sparkles: 3, max_rows: 2 })).toEqual(["Up to 2 rows"]);
    expect(limitPhrases(undefined)).toEqual([]);
  });
});

describe("blockLabel and neverSayLabel", () => {
  it("names known blocks and passes an unknown one through", () => {
    expect(blockLabel("headline_metrics")).toBe("Number cards");
    expect(blockLabel("hologram")).toBe("hologram");
  });

  it("hides a pattern and keeps a phrase", () => {
    expect(neverSayLabel("re:\\bELC\\d{3,}\\b")).toBeNull();
    expect(neverSayLabel(" no optimizations ")).toBe("no optimizations");
  });
});
