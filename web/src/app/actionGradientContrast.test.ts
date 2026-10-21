import { readFileSync } from "node:fs";
import { join } from "node:path";

import { describe, expect, it } from "vitest";

import { contrastRatio, over, resolveColor, themesFrom, toHex, type RGB, type Theme } from "./cssContrast";

/**
 * The action gradient a label sits on (the composer's Send circle, the
 * empty-state prompt's Run button) is --gradient-action-fill: primary →
 * secondary like the brand's --gradient-action-primary, but between the
 * measured stops /theme.css derives (internal/httpapi theme_contrast.go). The
 * label sits on every point of it, and with dark text an sRGB blend can dip
 * darker mid-way than either end, so this measures the whole blend, sampled
 * finer than the 2.1rem Send circle's pixels, with this file's own evaluator.
 */

const APP_DIR = import.meta.dirname ?? __dirname;
const css = readFileSync(join(APP_DIR, "globals.css"), "utf8");
const themes = themesFrom(css);

const TEXT = 4.5;
const SAMPLES = 64;
const MEASURED_STOPS = new Set([
  "--color-primary-fill",
  "--color-primary-fill-hover",
  "--color-secondary-fill",
  "--color-secondary-fill-hover",
]);

/** The lowest contrast of `text` on any point of a blend from a to b, each point rounded to 8 bits. */
function blendMin(text: RGB, a: RGB, b: RGB): number {
  let lowest = Infinity;
  for (let i = 0; i <= SAMPLES; i++) {
    const t = i / SAMPLES;
    const p = [0, 1, 2].map((c) => Math.round((a[c] + (b[c] - a[c]) * t) * 255) / 255) as RGB;
    lowest = Math.min(lowest, contrastRatio(text, p));
  }
  return lowest;
}

const opaque = (value: string, theme: Theme): RGB => {
  const c = resolveColor(value, theme);
  expect(c[3], `${value} is opaque`).toBe(1);
  return over(c, [0, 0, 0]);
};

/** A gradient token's stops, each a var() of a measured -fill token. */
function stopsOf(token: string, theme: Theme): string[] {
  const value = theme[token];
  expect(value, `${token} is declared`).toBeDefined();
  expect(value).toMatch(/^linear-gradient\(/);
  const stops = [...value.matchAll(/var\((--[\w-]+)\)/g)].map((m) => m[1]);
  expect(stops).toHaveLength(2);
  for (const s of stops) expect(MEASURED_STOPS.has(s), `${token} stop ${s} is measured`).toBe(true);
  return stops;
}

describe("the action gradient holds its label at AA on every point", () => {
  it.each(["dark", "light"] as const)("fleet's own palette, %s", (mode) => {
    const theme = themes[mode];
    const text = opaque("var(--color-on-primary-fill)", theme);
    for (const token of ["--gradient-action-fill", "--gradient-action-fill-hover"]) {
      const [a, b] = stopsOf(token, theme).map((s) => opaque(`var(${s})`, theme));
      expect(blendMin(text, a, b), `${mode} ${token}`).toBeGreaterThanOrEqual(TEXT);
    }
  });

  // What /theme.css emits for each palette trait (TestPrimaryFillContract).
  type Entry = { name: string; fill: string; hover: string; text: string; secondary: string; secondary_fill: string; secondary_hover: string };
  const ENTRIES: Entry[] = JSON.parse(
    readFileSync(join(APP_DIR, "..", "..", "..", "testdata", "contracts", "theme-primary-fill.json"), "utf8"),
  );

  it("the contract carries the gradient's measured end", () => {
    expect(ENTRIES.length).toBeGreaterThan(5);
    for (const e of ENTRIES) {
      expect(e.secondary_fill, e.name).toMatch(/^#[0-9a-f]{6}$/);
      expect(e.secondary_hover, e.name).toMatch(/^#[0-9a-f]{6}$/);
    }
  });

  it.each(ENTRIES.map((e) => [e.name, e] as const))("%s", (_name, e) => {
    for (const mode of ["light", "dark"] as const) {
      const theme: Theme = {
        ...themes[mode],
        "--color-primary-fill": e.fill,
        "--color-primary-fill-hover": e.hover,
        "--color-on-primary-fill": e.text,
        "--color-secondary-fill": e.secondary_fill,
        "--color-secondary-fill-hover": e.secondary_hover,
      };
      const text = opaque("var(--color-on-primary-fill)", theme);
      for (const token of ["--gradient-action-fill", "--gradient-action-fill-hover"]) {
        const [a, b] = stopsOf(token, theme).map((s) => opaque(`var(${s})`, theme));
        expect(blendMin(text, a, b), `${mode} ${token}`).toBeGreaterThanOrEqual(TEXT);
      }
    }
  });

  it("a pair of AA ends is not enough: the blend is what is measured", () => {
    const e = ENTRIES.find((x) => x.name === "a blend that dips between AA ends");
    expect(e).toBeDefined();
    if (!e) return;
    const text = opaque(e.text, {});
    const fill = opaque(e.fill, {});
    const raw = opaque(e.secondary, {});
    expect(contrastRatio(text, fill)).toBeGreaterThanOrEqual(TEXT);
    expect(contrastRatio(text, raw)).toBeGreaterThanOrEqual(TEXT);
    expect(blendMin(text, fill, raw)).toBeLessThan(TEXT);
    expect(blendMin(text, fill, opaque(e.secondary_fill, {}))).toBeGreaterThanOrEqual(TEXT);
  });

  it("fleet's own gradient end in globals.css is the one the server derives", () => {
    const own = ENTRIES.find((x) => x.name === "fleet's own palette");
    expect(own).toBeDefined();
    for (const mode of ["light", "dark"] as const) {
      expect(toHex(opaque("var(--color-secondary)", themes[mode]))).toBe(own?.secondary);
      expect(toHex(opaque("var(--color-secondary-fill)", themes[mode]))).toBe(own?.secondary_fill);
      expect(toHex(opaque("var(--color-secondary-fill-hover)", themes[mode]))).toBe(own?.secondary_hover);
    }
  });
});
