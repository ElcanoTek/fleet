// cssContrast — a small, dependency-free evaluator for the CSS color values
// globals.css declares, so contrast tests can measure the colors the browser
// will actually paint instead of a hand-copied hex table that drifts.
//
// It understands exactly what the token blocks use: hex, rgb()/rgba(),
// `transparent`, `var(--x[, fallback])` and `color-mix(in srgb, …)` (nested),
// with color-mix's alpha handling as CSS Color 5 specifies it (premultiplied
// interpolation; percentages that sum under 100% scale the result's alpha).
// Anything else throws, so a token written in a new syntax fails the test
// loudly rather than being skipped. Test-only: nothing in the app imports it.

export type RGBA = [number, number, number, number];
export type RGB = [number, number, number];
export type Theme = Record<string, string>;

/** The custom properties declared directly in one top-level block of `css`. */
export function blockDeclarations(css: string, selector: string): Theme {
  const start = css.indexOf(`\n${selector} {`);
  if (start < 0) throw new Error(`no top-level "${selector} {" block`);
  const body = css.slice(css.indexOf("{", start) + 1, css.indexOf("\n}", start));
  const out: Theme = {};
  // Comments first: they may contain `;` and `--words`.
  const clean = body.replace(/\/\*[\s\S]*?\*\//g, "");
  for (const m of clean.matchAll(/(--[a-z0-9-]+)\s*:\s*([^;]+);/gi)) {
    out[m[1]] = m[2].replace(/\s+/g, " ").trim();
  }
  return out;
}

/** fleet's two themes: dark is the `:root` default, light overlays it. */
export function themesFrom(css: string): { dark: Theme; light: Theme } {
  const root = blockDeclarations(css, ":root");
  const light = blockDeclarations(css, ':root[data-theme="light"]');
  return { dark: { ...root }, light: { ...root, ...light } };
}

/** Split on commas that are not inside parentheses. */
function splitTopLevel(s: string): string[] {
  const parts: string[] = [];
  let depth = 0;
  let cur = "";
  for (const ch of s) {
    if (ch === "(") depth++;
    if (ch === ")") depth--;
    if (ch === "," && depth === 0) {
      parts.push(cur.trim());
      cur = "";
    } else cur += ch;
  }
  parts.push(cur.trim());
  return parts;
}

/** `name(args)` when the whole string is one call, else null. */
function call(s: string): { name: string; args: string } | null {
  const m = /^([a-z-]+)\(([\s\S]*)\)$/i.exec(s.trim());
  if (!m) return null;
  // Reject "a(...) b(...)" — the closing paren must match the first open.
  let depth = 0;
  for (let i = m[1].length; i < s.trim().length; i++) {
    const ch = s.trim()[i];
    if (ch === "(") depth++;
    if (ch === ")") depth--;
    if (depth === 0 && i < s.trim().length - 1) return null;
  }
  return { name: m[1].toLowerCase(), args: m[2] };
}

function hex(h: string): RGBA {
  let x = h.slice(1);
  if (x.length === 3 || x.length === 4) x = [...x].map((c) => c + c).join("");
  const n = (i: number) => parseInt(x.slice(i, i + 2), 16) / 255;
  return [n(0), n(2), n(4), x.length === 8 ? n(6) : 1];
}

/**
 * rgb()/rgba() in comma or space syntax, each channel a number (0-255) or a
 * percentage, alpha a number (0-1) or a percentage. Wrong arity or a
 * component that is not a finite number throws: a NaN channel would make
 * every ratio NaN, and `NaN < min` is false, so a broken token would pass.
 * Out-of-range values clamp, as CSS does.
 */
function rgbComponents(value: string, args: string): RGBA {
  const bad = () => new Error(`unsupported color value: ${value}`);
  // The two grammars the browser accepts, never mixed: legacy commas
  // `r, g, b[, a]`, or modern spaces with an optional slash `r g b[ / a]`.
  let parts: string[];
  if (args.includes(",")) {
    if (args.includes("/")) throw bad();
    parts = args.split(",").map((p) => p.trim());
    if (parts.some((p) => p === "" || /\s/.test(p))) throw bad();
  } else {
    const [rgb, alpha, ...rest] = args.split("/");
    if (rest.length) throw bad();
    parts = rgb.trim().split(/\s+/).filter(Boolean);
    if (parts.length !== 3) throw bad();
    if (alpha !== undefined) {
      const a = alpha.trim();
      if (!a || /\s/.test(a)) throw bad();
      parts.push(a);
    }
  }
  if (parts.length !== 3 && parts.length !== 4) throw bad();
  const num = (s: string, scale: number): number => {
    const pct = s.endsWith("%");
    const n = Number(pct ? s.slice(0, -1) : s);
    if (!/^[+-]?(\d+\.?\d*|\.\d+)%?$/.test(s) || !Number.isFinite(n)) {
      throw new Error(`unsupported color value: ${value}`);
    }
    return Math.min(Math.max(pct ? (n / 100) * scale : n, 0), scale);
  };
  return [num(parts[0], 255) / 255, num(parts[1], 255) / 255, num(parts[2], 255) / 255, parts[3] ? num(parts[3], 1) : 1];
}

/** Resolve a CSS color value against a theme's custom properties. */
export function resolveColor(value: string, theme: Theme, seen: string[] = []): RGBA {
  const v = value.trim();
  // CSS hex is 3, 4, 6 or 8 digits; a 5- or 7-digit typo is invalid CSS and
  // must fail here, not be read as some opaque color.
  if (/^#(?:[0-9a-f]{3,4}|[0-9a-f]{6}|[0-9a-f]{8})$/i.test(v)) return hex(v);
  if (v === "transparent") return [0, 0, 0, 0];
  const c = call(v);
  if (!c) throw new Error(`unsupported color value: ${v}`);
  if (c.name === "var") {
    const [name, ...fallback] = splitTopLevel(c.args);
    if (seen.includes(name)) throw new Error(`var() cycle through ${name}`);
    const decl = theme[name];
    if (decl !== undefined) return resolveColor(decl, theme, [...seen, name]);
    if (fallback.length) return resolveColor(fallback.join(","), theme, seen);
    throw new Error(`undefined custom property ${name}`);
  }
  if (c.name === "rgb" || c.name === "rgba") return rgbComponents(v, c.args);
  if (c.name === "color-mix") {
    const args = splitTopLevel(c.args);
    if (args.length !== 3) throw new Error(`unsupported color value: ${v}`);
    const [space, a, b] = args;
    if (space.replace(/\s+/g, " ") !== "in srgb") throw new Error(`color-mix space ${space} unsupported`);
    const stop = (s: string): { color: string; pct: number | null } => {
      const m = /^([\s\S]*?)\s+(\d+(?:\.\d+)?)%$/.exec(s.trim());
      return m ? { color: m[1], pct: Number(m[2]) / 100 } : { color: s.trim(), pct: null };
    };
    const s1 = stop(a);
    const s2 = stop(b);
    // CSS rejects a percentage over 100% and a pair that sums to 0%.
    if ((s1.pct ?? 0) > 1 || (s2.pct ?? 0) > 1) throw new Error(`unsupported color value: ${v}`);
    let p1 = s1.pct ?? (s2.pct === null ? 0.5 : 1 - s2.pct);
    let p2 = s2.pct ?? 1 - p1;
    const sum = p1 + p2;
    if (!(sum > 0)) throw new Error(`unsupported color value: ${v}`);
    const alphaScale = sum < 1 ? sum : 1;
    p1 /= sum;
    p2 /= sum;
    const c1 = resolveColor(s1.color, theme, seen);
    const c2 = resolveColor(s2.color, theme, seen);
    const alpha = c1[3] * p1 + c2[3] * p2;
    if (alpha === 0) return [0, 0, 0, 0];
    const ch = (i: number) => (c1[i] * c1[3] * p1 + c2[i] * c2[3] * p2) / alpha;
    return [ch(0), ch(1), ch(2), alpha * alphaScale];
  }
  throw new Error(`unsupported color function ${c.name}()`);
}

/** Paint `top` (possibly translucent) over an opaque color. */
export function over(top: RGBA, under: RGB): RGB {
  const a = top[3];
  return [0, 1, 2].map((i) => top[i] * a + under[i] * (1 - a)) as RGB;
}

/** Paint layers bottom-to-top over an opaque backdrop. */
export function paint(backdrop: RGB, ...layers: RGBA[]): RGB {
  return layers.reduce<RGB>((acc, layer) => over(layer, acc), backdrop);
}

function luminance([r, g, b]: RGB): number {
  const lin = (c: number) => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4);
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

/** WCAG 2.x contrast ratio between two opaque colors. */
export function contrastRatio(a: RGB, b: RGB): number {
  if (![...a, ...b].every(Number.isFinite)) throw new Error(`contrast of a non-finite color: ${a} / ${b}`);
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
}

export function toHex([r, g, b]: RGB): string {
  return `#${[r, g, b].map((c) => Math.round(c * 255).toString(16).padStart(2, "0")).join("")}`;
}
