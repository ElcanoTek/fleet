import { readFileSync, readdirSync, statSync } from "node:fs";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

/**
 * Design-token guards (#890).
 *
 * web/src/app/DESIGN.md states the rule as "no raw colors in .tsx". It used to
 * say "no raw *hex* colors", and that wording is why this bug shipped: every
 * offending site spelled its color as a Tailwind utility (`text-white`), not a
 * hex literal, so it slipped past the rule, past review, and past every test.
 *
 * The concrete failure: 16 elements painted `text-white` on a token-driven fill.
 * A bundle that themes `--color-primary` to a light hue (yellow, lime, amber,
 * cyan) rendered white-on-light — the Reklaim deployment's login button measured
 * 1.33:1 against WCAG AA's 4.5:1. Three of the four fill colors involved failed
 * contrast, and two of them failed for *fleet's own* stock palette, not only for
 * a white-labeled one:
 *
 *   white on --color-primary (Reklaim #FFDF03) ....  1.33:1  ✗
 *   white on --color-accent  (fleet dark #9da7ef) .  2.28:1  ✗  (stock fleet)
 *   white on --color-danger  (fleet dark #e08080) .  2.77:1  ✗  (stock fleet)
 *
 * So the fix could not be "swap one token": each fill needs the foreground that
 * is readable against it in BOTH themes.
 *
 *   primary fill → var(--color-on-primary)   the purpose-built token (#889)
 *   accent fill  → var(--color-surface-1)    5.88–7.22:1, and already the
 *   danger fill  → var(--color-surface-1)    codebase's idiom at 3 other sites
 *
 * `--color-surface-1` works as a foreground because it inverts with the theme:
 * near-black in dark mode, white in light mode — exactly the polarity a
 * saturated fill needs. It is themable, so a bundle keeps control.
 *
 * The ban below is absolute rather than a "text-white next to bg-primary"
 * co-occurrence check, for two reasons: an absolute rule cannot be defeated by
 * splitting the classes across a parent and child (ConversationSidebar's tick
 * icon was exactly that shape — the fill was on the <span>, the `text-white` on
 * the <Icon> inside it), and every `text-white` in this app was on a token fill,
 * so there is no legitimate remaining use to carve out. If a future surface
 * genuinely needs white, add a semantic token for it — which is what DESIGN.md
 * already tells you to do for any color that has no token.
 */

const APP_DIR = join(import.meta.dirname ?? __dirname);

function tsxFiles(dir: string, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    if (entry === "node_modules" || entry === ".next") continue;
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) tsxFiles(full, out);
    else if (entry.endsWith(".tsx")) out.push(full);
  }
  return out;
}

/** Tailwind utilities that hardcode a color instead of referencing a token. */
const BANNED = ["text-white", "text-black"];

describe("design tokens", () => {
  const files = tsxFiles(APP_DIR);

  it("finds .tsx files to check (guards against a broken walk)", () => {
    // A silently-empty file list would make every assertion below vacuous.
    expect(files.length).toBeGreaterThan(50);
  });

  it.each(BANNED)("no .tsx hardcodes %s — use a token foreground", (banned) => {
    // Word-boundary matched so `text-white` does not also flag a hypothetical
    // `text-whitesmoke`, and prefixed variants (hover:, dark:, md:) are caught.
    const re = new RegExp(`(^|[\\s"'\`:])${banned}(?=[\\s"'\`]|$)`);
    const offenders: string[] = [];

    for (const file of files) {
      const lines = readFileSync(file, "utf8").split("\n");
      lines.forEach((line, i) => {
        if (re.test(line)) offenders.push(`${file.replace(APP_DIR, "")}:${i + 1}`);
      });
    }

    expect(offenders, `${banned} must be a token: ${offenders.join(", ")}`).toEqual([]);
  });

  it("every primary-filled element declares the on-primary foreground", () => {
    // Narrower, positive check: a `bg-[var(--color-primary)]` in the same class
    // string as a text color must use --color-on-primary. This catches the
    // regression where someone reaches for --color-text-primary (a foreground
    // for ordinary surfaces, which on a primary fill is near-invisible) instead.
    //
    // Only a BARE fill counts. A variant-prefixed one paints something other
    // than this element's own background, so the element's text color says
    // nothing about its readability — `before:bg-[var(--color-primary)]` is
    // NavRail's 2px active-indicator bar, sitting on an element whose real fill
    // is an 18%-alpha tint that --color-text-primary reads correctly against.
    // Matching it would be a false positive.
    const bareFill = /(^|[\s"'`])bg-\[var\(--color-primary\)\]/;
    const offenders: string[] = [];

    for (const file of files) {
      const lines = readFileSync(file, "utf8").split("\n");
      lines.forEach((line, i) => {
        if (!bareFill.test(line)) return;
        const textToken = /text-\[var\(--color-([a-z0-9-]+)\)\]/.exec(line);
        if (textToken && textToken[1] !== "on-primary") {
          offenders.push(`${file.replace(APP_DIR, "")}:${i + 1} → --color-${textToken[1]}`);
        }
      });
    }

    expect(offenders, `use --color-on-primary on a primary fill: ${offenders.join(", ")}`).toEqual(
      [],
    );
  });
});
