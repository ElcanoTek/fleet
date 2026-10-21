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
 *   primary fill → var(--color-on-primary-fill) on var(--color-primary-fill):
 *                  on_primary (#889) measured against a derived fill (#1815)
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

function walk(dir: string, keep: (entry: string) => boolean, out: string[] = []): string[] {
  for (const entry of readdirSync(dir)) {
    if (entry === "node_modules" || entry === ".next") continue;
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) walk(full, keep, out);
    else if (keep(entry)) out.push(full);
  }
  return out;
}

const tsxFiles = (dir: string) => walk(dir, (entry) => entry.endsWith(".tsx"));

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

  it("every primary-filled element declares the on-primary-fill foreground", () => {
    // Narrower, positive check: a `bg-[var(--color-primary-fill)]` in the same
    // class string as a text color must use --color-on-primary-fill. This
    // catches the regression where someone reaches for --color-text-primary (a
    // foreground for ordinary surfaces, which on a primary fill is
    // near-invisible) instead.
    //
    // Only a BARE fill counts. A variant-prefixed one paints something other
    // than this element's own background, so the element's text color says
    // nothing about its readability.
    const bareFill = /(^|[\s"'`])bg-\[var\(--color-primary-fill\)\]/;
    const offenders: string[] = [];

    for (const file of files) {
      const lines = readFileSync(file, "utf8").split("\n");
      lines.forEach((line, i) => {
        if (!bareFill.test(line)) return;
        const textToken = /(^|[\s"'`])text-\[var\(--color-([a-z0-9-]+)\)\]/.exec(line);
        if (textToken && textToken[2] !== "on-primary-fill") {
          offenders.push(`${file.replace(APP_DIR, "")}:${i + 1} → --color-${textToken[2]}`);
        }
      });
    }

    expect(
      offenders,
      `use --color-on-primary-fill on a primary fill: ${offenders.join(", ")}`,
    ).toEqual([]);
  });
});

/**
 * Filled buttons are held to WCAG AA for any palette (#1815 follow-up).
 *
 * White on fleet's own `--color-primary` (#7272ab, which the elcano bundle
 * repeats) is 4.46:1, just under AA's 4.5:1 for text, and a bundle's brand pair
 * is not measured at all. `--color-primary-hover` is worse as a hover: it is
 * the DEEPER primary in light but the LIGHTER one in dark (white on fleet's
 * dark #8686c4 is 3.38:1), and a `hover:opacity-90` fades the fill toward the
 * page. #1815 added the measured pair — `--color-primary-fill` /
 * `-fill-hover` under `--color-on-primary-fill`, which `/theme.css` derives
 * for every bundle palette (internal/httpapi/theme_contrast.go) — and moved
 * the approval cards to it. These rules hold the whole of web/src to it: every
 * filled button, chip, selected option, switch knob and icon on the brand
 * fill paints the measured pair, so no foreground anywhere sits on the raw
 * brand pair.
 *
 * The raw `--color-primary` stays legal where nothing is painted on it: tints
 * (`color-mix(…, transparent)`), borders, `accent-[…]` and focus colors, the
 * loading logo, and a nav row's 2px active bar (NAV_BAR_FILES).
 *
 * Co-occurrence rules (a fill with a faded hover, a disableable button with a
 * bare hover, a label on the action gradient) read more than the fill's own
 * line, because this codebase often splits one element's classes across the
 * strings of a `[…].join(" ")` array, and puts `disabled={…}` on its own line:
 *
 *   - "class" extent: the fill's line plus the continuation lines of the same
 *     class list, stopping at the start of another one (a `const`, a ternary
 *     branch `? "…"` / `: "…"`, a `className=` or a JSX tag);
 *   - "element" extent: the JSX element the line sits in, from its `<tag` to
 *     the line that closes the opening tag.
 */
const SRC_DIR = join(APP_DIR, "..");
const sourceFilesUnder = (dir: string) =>
  walk(dir, (entry) => /\.tsx?$/.test(entry) && !/\.(test|spec)\.tsx?$/.test(entry));

/**
 * The nav rows whose active state is a `before:` bar on the raw primary: a
 * 2px pseudo-element with no content text, on a row whose real fill is a
 * translucent tint. Allowed only in these files, and each must still use it.
 */
const NAV_BAR_FILES = ["app/shared/ui/NavRail.tsx", "app/help/ui/GuideNav.tsx", "app/settings/SetNav.tsx"];
const NAV_BAR = "before:bg-[var(--color-primary)]";

const rel = (file: string) => file.slice(SRC_DIR.length + 1);

/** Every spelling of a fill painted from the raw brand primary (or its hover). */
const RAW_FILL = [
  /(^|[\s"'`:])bg-\[var\(--color-primary(-hover)?\)\]/,
  /(^|[\s"'`:])bg-\(--color-primary(-hover)?\)/,
  /\b(background|backgroundColor)\s*:\s*["'`]var\(--color-primary(-hover)?\)/,
];
/** Every spelling of the unmeasured on-primary foreground. */
const RAW_LABEL = [
  /(^|[\s"'`:])(text|fill|stroke)-\[var\(--color-on-primary\)\]/,
  /(^|[\s"'`:])(text|fill|stroke)-\(--color-on-primary\)/,
  /\bcolor\s*:\s*["'`]var\(--color-on-primary\)/,
];

type Extent = "class" | "element";
type Rule = { why: string; line: RegExp[]; window?: RegExp[]; extent?: Extent };

/**
 * `line` patterns are tested against the line; when `window` is set, the line
 * must match a `line` pattern AND some line of its extent must match a
 * `window` pattern.
 */
const CLASS_LIST_START = /^\s*(const |let |export |[?:] ["'`]|className=|<[A-Za-z])/;
const MAX_EXTENT = 12;

function extentOf(lines: string[], i: number, kind: Extent): string[] {
  if (kind === "class") {
    let start = i;
    while (start > 0 && i - start < MAX_EXTENT && !CLASS_LIST_START.test(lines[start])) start--;
    let end = i;
    while (end + 1 < lines.length && end - i < MAX_EXTENT && !CLASS_LIST_START.test(lines[end + 1])) end++;
    return lines.slice(start, end + 1);
  }
  let start = i;
  while (start > 0 && i - start < MAX_EXTENT && !/<[A-Za-z]/.test(lines[start])) start--;
  if (!/<[A-Za-z]/.test(lines[start])) return [lines[i]];
  let end = i;
  while (end + 1 < lines.length && end - i < MAX_EXTENT && !/(^|[^=])>\s*$/.test(lines[end])) end++;
  return lines.slice(start, end + 1);
}
const RULES: Rule[] = [
  {
    why: "a foreground on the brand fill is --color-on-primary-fill (on_primary painted solid, measured with the fill)",
    line: RAW_LABEL,
  },
  {
    why: "a fill is --color-primary-fill, hovering to --color-primary-fill-hover (the brand pair can miss 4.5:1, and -hover is lighter in dark)",
    line: RAW_FILL,
  },
  {
    why: "a foreground on a fill painted from the raw primary (a gradient too): use the -fill tokens",
    line: [/var\(--color-primary(-hover)?\)(?=.*--color-on-primary)|--color-on-primary.*var\(--color-primary(-hover)?\)/],
  },
  {
    why: "a hover that fades or lightens a filled button lowers its contrast: hover to --color-primary-fill-hover",
    line: [/--color-primary-fill\b/],
    window: [/(^|[\s"'`:])hover:(opacity|brightness)-/],
    extent: "class",
  },
  {
    why: "a disableable button must not deepen on hover while disabled: use enabled:hover:",
    line: [/(^|[\s"'`])hover:bg-\[var\(--color-primary-fill-hover\)\]/],
    window: [/(^|[\s"'`:])disabled:|\bdisabled=\{/],
    extent: "element",
  },
  {
    why: "a foreground on --gradient-action-primary: its secondary stop is not measured, use the -fill tokens",
    line: [/--gradient-action-primary/],
    window: [/--color-on-primary/],
    extent: "class",
  },
];

function offendersIn(name: string, lines: string[], used?: Set<string>): string[] {
  const out: string[] = [];
  const navFile = NAV_BAR_FILES.includes(name);
  const prepared = lines.map((raw) => {
    if (navFile && raw.includes(NAV_BAR) && !/before:(content|text)-/.test(raw)) {
      used?.add(name);
      return raw.replaceAll(NAV_BAR, "");
    }
    return raw;
  });
  prepared.forEach((line, i) => {
    for (const rule of RULES) {
      if (!rule.line.some((re) => re.test(line))) continue;
      const windowRules = rule.window;
      const near = rule.extent ? extentOf(prepared, i, rule.extent) : [line];
      if (windowRules && !near.some((l) => windowRules.some((re) => re.test(l)))) continue;
      out.push(`${name}:${i + 1} — ${rule.why}`);
    }
  });
  return out;
}

describe("filled primary buttons use the measured fill pair (WCAG AA)", () => {
  const files = sourceFilesUnder(SRC_DIR);

  it("walks all of web/src (guards against a broken walk)", () => {
    expect(files.length).toBeGreaterThan(100);
    expect(files.some((f) => rel(f) === "proxy.ts")).toBe(true);
  });

  it("no foreground sits on the raw brand fill, and no filled button fades on hover", () => {
    const offenders: string[] = [];
    const used = new Set<string>();
    for (const file of files) offenders.push(...offendersIn(rel(file), readFileSync(file, "utf8").split("\n"), used));
    expect(offenders).toEqual([]);
    // A stale allowance would silently widen what the guard accepts.
    expect(NAV_BAR_FILES.filter((f) => !used.has(f))).toEqual([]);
  });

  it("no globals.css rule paints a foreground on the raw brand fill", () => {
    const css = readFileSync(join(APP_DIR, "globals.css"), "utf8").replace(/\/\*[\s\S]*?\*\//g, "");
    const offenders: string[] = [];
    // Innermost blocks only: comments are stripped, so a `{…}` with no brace
    // inside is one rule (nested @layer/@media wrappers fall away).
    for (const m of css.matchAll(/([^{};]+)\{([^{}]*)\}/g)) {
      const selector = m[1].trim();
      const body = m[2];
      const rawBg = /(^|[\s;])background(-color)?\s*:\s*var\(--color-primary(-hover)?\)\s*(!important\s*)?(;|$)/.test(body);
      const onPrimary = /(^|[\s;])(color|background(-color)?|fill|stroke)\s*:\s*var\(--color-on-primary\)/.test(body);
      const fillLabel = /(^|[\s;])color\s*:\s*var\(--color-on-primary-fill\)/.test(body);
      const fillBg = /(^|[\s;])background(-color)?\s*:\s*var\(--color-primary-fill(-hover)?\)/.test(body);
      const anyBg = /(^|[\s;])background(-color|-image)?\s*:/.test(body);
      if (onPrimary) offenders.push(`${selector} — a foreground on the brand fill is --color-on-primary-fill`);
      if (fillLabel && anyBg && !fillBg) offenders.push(`${selector} — --color-on-primary-fill is measured on --color-primary-fill only`);
      if (rawBg) offenders.push(`${selector} — a fill is --color-primary-fill (hover: -fill-hover)`);
    }
    expect(offenders).toEqual([]);
  });

  it("the rules catch the shapes they ban, across lines too (mutation check)", () => {
    const bad: string[][] = [
      ['className="bg-[var(--color-primary)] text-[var(--color-on-primary)]"'],
      ['className="bg-(--color-primary) text-(--color-on-primary)"'],
      ['style={{ background: "var(--color-primary)", color: "var(--color-on-primary)" }}'],
      ['className="bg-[var(--color-primary-fill)] text-[var(--color-on-primary-fill)] hover:opacity-90"'],
      ['className="bg-[var(--color-primary-fill)] text-[var(--color-on-primary-fill)] hover:bg-[var(--color-primary-hover)]"'],
      ['className="bg-[linear-gradient(140deg,var(--color-primary),var(--color-primary-hover))] text-[var(--color-on-primary-fill)]"'],
      ['className="bg-[var(--color-primary-fill)] text-[var(--color-on-primary-fill)] hover:bg-[var(--color-primary-fill-hover)] disabled:opacity-50"'],
      ['className="bg-[var(--color-primary-fill)] text-[var(--color-on-primary-fill)] hover:brightness-[1.08]"'],
      // One element's classes split across a [...].join(" ") array.
      ["className={[", '  "bg-[var(--color-primary-fill)] text-[var(--color-on-primary-fill)]",', '  "hover:opacity-90",', '].join(" ")}'],
      ["<button", "  disabled={busy}", '  className="bg-[var(--color-primary-fill)] hover:bg-[var(--color-primary-fill-hover)]"', ">"],
      ['className="bg-[image:var(--gradient-action-primary)]', '  text-[var(--color-on-primary-fill)]"'],
      // A nav bar allowance outside the nav files, or one carrying text.
      [`className="${NAV_BAR} before:content-['New'] before:text-[var(--color-on-primary-fill)]"`],
    ];
    for (const lines of bad) expect(offendersIn("app/x.tsx", lines), lines.join("\n")).not.toEqual([]);
    expect(offendersIn(NAV_BAR_FILES[0], [`className="${NAV_BAR} before:content-['New']"`])).not.toEqual([]);

    const good: string[][] = [
      ['className="bg-[var(--color-primary-fill)] text-[var(--color-on-primary-fill)] enabled:hover:bg-[var(--color-primary-fill-hover)] disabled:opacity-50"'],
      ['className="border-[var(--color-primary)] bg-[color-mix(in_srgb,var(--color-primary)_14%,transparent)] text-[var(--color-text-primary)]"'],
      ['className="accent-[var(--color-primary)]"'],
      ['className="bg-[linear-gradient(var(--color-surface-1),var(--color-surface-1))_padding-box,var(--gradient-action-primary)_border-box]"'],
    ];
    for (const lines of good) expect(offendersIn("app/x.tsx", lines), lines.join("\n")).toEqual([]);
    expect(offendersIn(NAV_BAR_FILES[0], [`className="before:h-[0.85rem] ${NAV_BAR} max-[900px]:before:hidden"`])).toEqual([]);
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
