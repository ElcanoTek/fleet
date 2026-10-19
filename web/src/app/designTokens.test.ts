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

/**
 * Status-tint contrast (genui callout bug). GenerativeCard painted
 * `--color-warning-soft` — a TEXT color — as a callout's fill, so a warning
 * callout rendered pale text on a solid pastel: 1.14:1 for its title and 1.38:1
 * for its body. And the -soft values themselves did not all clear AA on the
 * 15% -strong tint DESIGN.md prescribes for them (light warning 3.4:1, dark
 * danger 4.0:1), so NoticeBanner and StatusChip carried the same gap at a
 * smaller scale.
 *
 * Status colors are deliberately NOT bundle-themable (config/default
 * manifest.yaml), but the surfaces they sit on are. So the check runs against
 * fleet's own surfaces plus the darkest and lightest surfaces the shipped
 * client bundles declare (elcano, raptive, omnicom, reklaim, zeta and the two
 * public examples, as of 2026-10-19): a bundle that themes a surface outside
 * this envelope should add it here.
 */
const STATUS_SURFACES = {
  dark: ["#1a0b1e", "#241b31", "#2f2741", "#07071c", "#0a0908", "#1e2330", "#19193a"],
  light: ["#f4f6fb", "#ffffff", "#e9eefc", "#e9e9e9", "#ebebeb", "#f0edeb", "#f2f0ed"],
} as const;

function hexRGB(hex: string): [number, number, number] {
  const h = hex.replace("#", "");
  return [0, 2, 4].map((i) => parseInt(h.slice(i, i + 2), 16) / 255) as [number, number, number];
}

function luminance([r, g, b]: [number, number, number]): number {
  const lin = (c: number) => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4);
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

function contrast(a: [number, number, number], b: [number, number, number]): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

/** `color-mix(in srgb, fg <alpha>, transparent)` composited over bg. */
function tint(fg: [number, number, number], bg: [number, number, number], alpha: number): [number, number, number] {
  return fg.map((c, i) => c * alpha + bg[i] * (1 - alpha)) as [number, number, number];
}

/** The status tokens declared in one top-level globals.css block. */
function statusTokens(css: string, selector: string): Record<string, string> {
  const start = css.indexOf(`${selector} {`);
  expect(start, `${selector} block in globals.css`).toBeGreaterThanOrEqual(0);
  const block = css.slice(start, css.indexOf("\n}", start));
  const out: Record<string, string> = {};
  for (const m of block.matchAll(/--color-(success|warning|danger)-(strong|soft):\s*(#[0-9a-fA-F]{6})\s*;/g)) {
    out[`${m[1]}-${m[2]}`] = m[3];
  }
  return out;
}

describe("status tint contrast", () => {
  const css = readFileSync(join(APP_DIR, "globals.css"), "utf8");
  const themes = { dark: statusTokens(css, ":root"), light: statusTokens(css, ':root[data-theme="light"]') };

  it.each(["dark", "light"] as const)("every -soft clears AA on its own -strong tint (%s)", (mode) => {
    const tokens = themes[mode];
    const failures: string[] = [];
    for (const tone of ["success", "warning", "danger"]) {
      const strong = tokens[`${tone}-strong`];
      const soft = tokens[`${tone}-soft`];
      expect(strong && soft, `${mode} ${tone} -strong/-soft parsed`).toBeTruthy();
      // 15% is the documented tint; 20% covers the heavier tints in use (18%).
      for (const alpha of [0.15, 0.2]) {
        for (const surface of STATUS_SURFACES[mode]) {
          const ratio = contrast(hexRGB(soft), tint(hexRGB(strong), hexRGB(surface), alpha));
          if (ratio < 4.5) failures.push(`${tone}-soft ${soft} on ${alpha * 100}% ${strong} over ${surface}: ${ratio.toFixed(2)}:1`);
        }
      }
    }
    expect(failures, "WCAG AA is 4.5:1 for body text").toEqual([]);
  });

  it("no .tsx paints a -soft status token as a fill — it is a text color", () => {
    const res = [
      /bg-\[var\(--color-(success|warning|danger)-soft\)\]/,
      /\b(bg|background|backgroundColor)\s*:\s*["'`]var\(--color-(success|warning|danger)-soft\)/,
    ];
    const offenders: string[] = [];
    for (const file of tsxFiles(APP_DIR)) {
      readFileSync(file, "utf8")
        .split("\n")
        .forEach((line, i) => {
          if (res.some((re) => re.test(line))) offenders.push(`${file.replace(APP_DIR, "")}:${i + 1}`);
        });
    }
    expect(offenders).toEqual([]);
  });
});
