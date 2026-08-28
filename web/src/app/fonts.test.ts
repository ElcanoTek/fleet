import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

import { describe, expect, it } from "vitest";

/**
 * Font guards.
 *
 * Elcano ships exactly two typefaces, everywhere: Nebula Sans (SIL OFL 1.1) for
 * UI/body/headings and Hack (MIT, plus Bitstream Vera for its Vera-derived
 * glyphs) for code, logs and tabular output. `src/app/fonts/fonts.css` — a
 * vendored copy of flag's `design-system/fonts/fonts.css` — is the ONLY place
 * in this app a font family is named; `globals.css` imports it and reads the
 * three `--font-*-brand` variables it sets.
 *
 * Four things here are not self-enforcing, and each has already gone wrong
 * somewhere in this org:
 *
 *  1. **A third face creeps in.** Every face this repo has ever shipped came in
 *     as "just one more @font-face". The check below fails on any family in
 *     fonts.css other than the two, and on any of the retired names (or a font
 *     CDN host) appearing anywhere under src/.
 *  2. **A licence file gets dropped.** Both licences REQUIRE their text to
 *     travel with the binaries, so deleting OFL.txt/LICENSE.md to "tidy up"
 *     makes this MIT repo's distribution non-compliant. It is a legal defect
 *     that no build or lint would ever notice.
 *  3. **A url() drifts off a real file.** The bundler resolves these, so a
 *     moved binary is normally a build error — but only for the sheet the build
 *     actually imports. Checking the paths here names the file instead of
 *     failing inside a CSS loader.
 *  4. **Tabular figures get removed.** Nebula Sans has PROPORTIONAL figures
 *     (digit advances 407–625 per 1000 em units) where IBM Plex Sans had every
 *     digit at 600, so numeric columns in the sans face no longer align for
 *     free. Measured in Chromium, an 8-digit table cell rendered all-`1`s
 *     versus all-`8`s differs by 25.09px without tabular figures and 0.11px
 *     with them. The base rule is what keeps a numeric column added later from
 *     landing ragged by omission, so its deletion must fail loudly.
 */

const APP_DIR = dirname(new URL(import.meta.url).pathname);
const FONTS_DIR = join(APP_DIR, "fonts");
const FONTS_CSS = join(FONTS_DIR, "fonts.css");
const GLOBALS_CSS = join(APP_DIR, "globals.css");

/** The only two families this org ships. */
const ALLOWED_FAMILIES = ["Nebula Sans", "Hack"];

/**
 * Faces retired from the org's products, plus the two font CDNs. Dubai is
 * PROPRIETARY and must never appear in this MIT repo at all; the rest are
 * merely no longer shipped. Matched case-insensitively.
 */
const BANNED_PATTERNS = [
  /IBM\s*Plex/i,
  /\bDubai\b/i,
  /Share\s*Tech/i,
  /\bVT323\b/i,
  /"Inter"|'Inter'/,
  /fonts\.googleapis\.com/i,
  /fonts\.gstatic\.com/i,
];

function sourceFiles(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    if (entry === "node_modules" || entry === ".next") continue;
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) sourceFiles(full, out);
    // fonts.css is exempt from the banned-name scan below (not from anything
    // else): its header records WHY the org retired Dubai, IBM Plex, Share Tech
    // Mono and VT323, and that history is the most useful comment in the file.
    // The @font-face check above already pins that sheet to exactly two
    // families, which is the guard that matters there.
    else if (
      /\.(tsx?|css|mjs|json)$/.test(entry) &&
      !full.endsWith("fonts.test.ts") &&
      full !== FONTS_CSS
    ) {
      out.push(full);
    }
  }
  return out;
}

describe("the font sheet ships exactly two faces", () => {
  const css = readFileSync(FONTS_CSS, "utf8");

  it("declares @font-face for Nebula Sans and Hack, and nothing else", () => {
    const families = [...css.matchAll(/font-family:\s*"([^"]+)"/g)].map((m) => m[1]);
    expect(families.length).toBeGreaterThan(0);
    expect([...new Set(families)].sort()).toEqual([...ALLOWED_FAMILIES].sort());
  });

  it("binds the three brand variables globals.css reads", () => {
    expect(css).toMatch(/--font-brand:\s*"Nebula Sans"/);
    expect(css).toMatch(/--font-code-brand:\s*"Hack"/);
    expect(css).toMatch(/--font-code-ui-brand:\s*"Hack"/);
  });

  it("points every url() at a file that exists", () => {
    const urls = [...css.matchAll(/url\("([^"]+)"\)/g)].map((m) => m[1]);
    // 6 Nebula weights/styles + 4 Hack faces × 2 formats.
    expect(urls.length).toBe(14);
    for (const u of urls) {
      expect(existsSync(resolve(FONTS_DIR, u)), `fonts.css references a missing file: ${u}`).toBe(
        true,
      );
    }
  });

  it("keeps each licence text beside the binaries it covers", () => {
    // Both the OFL and the MIT/Bitstream-Vera licences require the notice to
    // travel with the font. Deleting either is a distribution defect.
    expect(existsSync(join(FONTS_DIR, "nebula-sans", "OFL.txt"))).toBe(true);
    expect(existsSync(join(FONTS_DIR, "hack", "LICENSE.md"))).toBe(true);
  });
});

describe("no retired face and no font CDN anywhere under src/", () => {
  const files = sourceFiles(APP_DIR.replace(/\/app$/, ""));

  it("scans a non-trivial number of files", () => {
    expect(files.length).toBeGreaterThan(50);
  });

  for (const pattern of BANNED_PATTERNS) {
    it(`has no match for ${pattern}`, () => {
      const hits = files.filter((f) => pattern.test(readFileSync(f, "utf8")));
      expect(hits, `${pattern} still appears in:\n  ${hits.join("\n  ")}`).toEqual([]);
    });
  }
});

describe("tabular figures survive", () => {
  const css = readFileSync(GLOBALS_CSS, "utf8");

  it("keeps tabular figures the base default for every table", () => {
    // Nebula Sans's proportional digits make this load-bearing, not cosmetic.
    expect(css).toMatch(/table\s*\{\s*font-variant-numeric:\s*tabular-nums;\s*\}/);
  });

  it("keeps it on the numeric readouts that sit outside a table", () => {
    for (const cls of [
      "usage-tile-value",
      "sleeping-tasks-count",
      "task-count-chip",
      "task-card-time",
      "sla-detail",
    ]) {
      expect(css, `${cls} lost its tabular figures`).toMatch(
        new RegExp(`\\.${cls}[^{]*\\{[^}]*font-variant-numeric:\\s*tabular-nums`, "s"),
      );
    }
  });
});
