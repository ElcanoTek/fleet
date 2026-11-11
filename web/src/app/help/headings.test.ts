import { describe, expect, it } from "vitest";
import { GUIDES, readGuide } from "./guideContent";
import { LANGUAGES } from "@/app/lib/i18n";
import { extractHeadings, slugify } from "./headings";

describe("slugify", () => {
  it("matches the anchor form the guides' own links are written in", () => {
    expect(slugify("4. Run states")).toBe("4-run-states");
    expect(slugify("Keeping, shaping, and sharing conversations")).toBe(
      "keeping-shaping-and-sharing-conversations",
    );
    expect(slugify("Cards that ask for a decision")).toBe("cards-that-ask-for-a-decision");
  });
});

describe("extractHeadings", () => {
  it("collects h2 and h3 in document order, ignoring h1", () => {
    const headings = extractHeadings("# Title\n\n## One\n\ntext\n\n### Under one\n\n## Two\n");
    expect(headings).toEqual([
      { id: "one", text: "One", depth: 2 },
      { id: "under-one", text: "Under one", depth: 3 },
      { id: "two", text: "Two", depth: 2 },
    ]);
  });

  it("does not mistake a comment inside a fenced block for a heading", () => {
    const headings = extractHeadings("## Real\n\n```sh\n## not a heading\n```\n\n## Also real\n");
    expect(headings.map((h) => h.text)).toEqual(["Real", "Also real"]);
  });
});

describe("the shipped guides", () => {
  it.each(GUIDES.map((g) => g.slug))("%s has sections to navigate", (slug) => {
    const guide = GUIDES.find((g) => g.slug === slug)!;
    const headings = extractHeadings(readGuide(guide));
    expect(headings.filter((h) => h.depth === 2).length).toBeGreaterThan(3);
  });

  // The guides cross-reference each other's sections constantly ("see [Run
  // states](#4-run-states)"). A renamed heading silently turns every one of
  // those into a link that scrolls nowhere — invisible in review, and only
  // ever found by the reader it fails. So: every in-document anchor a guide
  // links to must be a heading that guide actually has.
  it.each(GUIDES.map((g) => g.slug))("%s has no dangling in-page links", (slug) => {
    const guide = GUIDES.find((g) => g.slug === slug)!;
    const content = readGuide(guide);
    const ids = new Set(extractHeadings(content).map((h) => h.id));
    const dangling = [...content.matchAll(/\]\(#([^)]+)\)/g)]
      .map((m) => m[1])
      .filter((anchor) => !ids.has(anchor));
    expect(dangling).toEqual([]);
  });
});

for (const { value: language } of LANGUAGES) {
  describe(`the shipped ${language} guides`, () => {
    it.each(GUIDES)("$slug retains canonical section anchors and link destinations", (guide) => {
      const english = readGuide(guide, "en");
      const content = readGuide(guide, language);
      const canonical = extractHeadings(english);
      const headings = extractHeadings(content, english);
      expect(headings.map(({ id, depth }) => ({ id, depth }))).toEqual(
        canonical.map(({ id, depth }) => ({ id, depth })),
      );
      const ids = new Set(headings.map(({ id }) => id));
      const dangling = [...content.matchAll(/\]\(#([^)]+)\)/g)]
        .map((match) => match[1]).filter((id) => !ids.has(id));
      expect(dangling).toEqual([]);
      const links = (value: string) => [...value.matchAll(/\]\(([^)]+)\)/g)].map((m) => m[1]);
      expect(links(content)).toEqual(links(english));
      if (language !== "en") {
        expect(content).not.toBe(english);
        expect(headings[0].text).not.toBe(canonical[0].text);
      }
    });

    it.each(GUIDES)("$slug preserves commands and code while translating prose", (guide) => {
      const code = (value: string) => [...value.matchAll(/```[\s\S]*?```|`[^`]+`/g)]
        .map((match) => match[0].replace(/\s+/g, " "));
      expect(code(readGuide(guide, language))).toEqual(code(readGuide(guide, "en")));
    });
  });
}
