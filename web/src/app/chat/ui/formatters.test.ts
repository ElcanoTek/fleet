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

  // Every case below left a stray markup character in the preview — the exact
  // thing this function exists to prevent.
  it("removes a horizontal rule instead of leaving one marker behind", () => {
    // Ordered after the bullet and emphasis rules, each of these lost one
    // marker to those and kept the rest: "***" came out as "*".
    for (const rule of ["***", "___", "---", "- - -", "* * *", "_ _ _"]) {
      expect(stripMarkdown(rule), rule).toBe("");
    }
    expect(stripMarkdown("Intro\n***\nOutro")).toBe("Intro Outro");
  });

  it("keeps a link label when the URL contains parentheses", () => {
    expect(
      stripMarkdown("[wiki](https://en.wikipedia.org/wiki/Foo_(bar)) is nice"),
    ).toBe("wiki is nice");
  });

  it("renders an escaped marker as the marker, not as emphasis", () => {
    // CommonMark shows *not bold*; the escapes were being consumed as if they
    // were emphasis, which dropped the asterisks and kept the backslashes.
    expect(stripMarkdown("He said: \\*not bold\\*")).toBe(
      "He said: *not bold*",
    );
    expect(stripMarkdown("a \\_b\\_ c")).toBe("a _b_ c");
  });
  // Item B-3, second pass: the strip removed asterisks but left LaTeX and
  // table syntax — the two kinds of markup an analytics reply is most likely
  // to end on, so the preview showed raw TeX and a row of pipes.
  it("reads LaTeX math as words and numbers", () => {
    expect(
      stripMarkdown(
        "$\\text{CPM} = 1{,}000 \\times \\frac{\\text{Cost}}{\\text{Impressions}}$",
      ),
    ).toBe("CPM = 1,000 × Cost/Impressions");
    expect(stripMarkdown("$$E = mc^2$$")).toBe("E = mc2");
    expect(stripMarkdown("Roughly $\\pi \\approx 3.14$ per unit")).toBe(
      "Roughly π ≈ 3.14 per unit",
    );
  });

  it("handles a math span the upstream truncation cut in half", () => {
    // The reported preview, verbatim: clipped mid-expression, so there is no
    // closing delimiter to pair with.
    expect(stripMarkdown("$\\text{CPM} = 1{,}000 \\times \\frac{…}")).toBe(
      "CPM = 1,000 × …",
    );
  });

  it("leaves prices alone — a $ is only math with TeX inside it", () => {
    expect(stripMarkdown("it costs $5 and $7")).toBe("it costs $5 and $7");
    expect(stripMarkdown("Spend was $1,000 last week")).toBe(
      "Spend was $1,000 last week",
    );
  });

  it("collapses a pipe table to its cells and drops the separator row", () => {
    expect(
      stripMarkdown("| Channel | Spend |\n| --- | --- |\n| Search | 1,000 |"),
    ).toBe("Channel Spend Search 1,000");
    // Alignment colons are separator syntax too.
    expect(
      stripMarkdown(
        "Totals below:\n\n| Channel | Spend | CPM |\n|:---|---:|:-:|\n| Search | $1,000 | $4.20 |",
      ),
    ).toBe("Totals below: Channel Spend CPM Search $1,000 $4.20");
  });

  it("keeps a lone shell pipe in prose", () => {
    // One pipe, no leading pipe: that is a command, not a table row.
    expect(stripMarkdown("run grep foo | wc -l")).toBe("run grep foo | wc -l");
  });
});
