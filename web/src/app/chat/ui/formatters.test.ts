import { describe, expect, it } from "vitest";
import { stripMarkdown } from "./formatters";

// Item B3: the project home's chat previews are plain text with no markdown
// pipeline behind them, so the server's raw last-message snippet reached the
// UI with its markup intact — the reported case was a preview reading
// `**Proposed Memory:**` complete with asterisks.
describe("stripMarkdown", () => {
  it("strips the reported case", () => {
    expect(stripMarkdown('**Proposed Memory:** "office is in Austin"')).toBe(
      'Proposed Memory: "office is in Austin"',
    );
  });

  it("keeps the words and drops the markers", () => {
    const cases: [string, string][] = [
      ["# Heading", "Heading"],
      ["## Deep heading", "Deep heading"],
      ["> quoted line", "quoted line"],
      ["- bullet one", "bullet one"],
      ["1. numbered", "numbered"],
      ["*emphasis*", "emphasis"],
      ["_emphasis_", "emphasis"],
      ["***both***", "both"],
      ["~~struck~~", "struck"],
      ["`code()`", "code()"],
      ["[label](https://example.com)", "label"],
      ["![alt text](chart.png)", "alt text"],
    ];
    for (const [input, want] of cases) {
      expect(stripMarkdown(input), input).toBe(want);
    }
  });

  it("collapses a multi-line snippet to one line", () => {
    expect(stripMarkdown("Revenue is up\n\n**12%** week over week.")).toBe(
      "Revenue is up 12% week over week.",
    );
  });

  it("unwraps a fenced code block to its contents", () => {
    expect(stripMarkdown("```python\nprint(1)\n```")).toBe("print(1)");
  });

  it("leaves plain text — and the You: prefix — alone", () => {
    expect(stripMarkdown("You: run the numbers for Q3")).toBe(
      "You: run the numbers for Q3",
    );
    expect(stripMarkdown("")).toBe("");
  });

  it("does not eat a bare asterisk or underscore mid-word", () => {
    // 2 * 3 is arithmetic, not emphasis; snake_case is a name, not markup.
    expect(stripMarkdown("2 * 3 = 6")).toBe("2 * 3 = 6");
    expect(stripMarkdown("call user_email_lookup")).toBe(
      "call user_email_lookup",
    );
  });
});
