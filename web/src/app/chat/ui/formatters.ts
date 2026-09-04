// Small numeric/byte formatters shared by the chat presentational
// components. Extracted verbatim from chat-experience.tsx (slice 3 of #169);
// they are pure functions with no React dependency, so they live in a plain
// .ts module that vitest can import without a DOM.

export function formatUsd(v: number): string {
  if (!v) return "$0.00";
  if (v < 0.01) return `$${v.toFixed(4)}`;
  return `$${v.toFixed(2)}`;
}

export function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  return `${Math.floor(ms / 60_000)}m ${Math.floor((ms % 60_000) / 1000)}s`;
}

export function formatTokens(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`;
  return `${(n / 1_000_000).toFixed(2)}M`;
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`;
  return `${(n / (1024 * 1024 * 1024)).toFixed(1)} GB`;
}

// ── LaTeX / TeX math ─────────────────────────────────────────────────────────
//
// A preview never renders math either, so `$…$` and `$$…$$` reached the UI as
// raw TeX: `$\text{CPM} = 1{,}000 \times \frac{\text{Cost}}{\text{Impressions}}$`.
// These two tables plus texToText are the smallest thing that turns that into
// words and numbers — NOT a renderer, exactly as the rest of this module is
// not a markdown parser.
const TEX_SYMBOLS: Record<string, string> = {
  times: "×",
  cdot: "·",
  div: "÷",
  pm: "±",
  mp: "∓",
  approx: "≈",
  neq: "≠",
  ne: "≠",
  equiv: "≡",
  le: "≤",
  leq: "≤",
  ge: "≥",
  geq: "≥",
  to: "→",
  rightarrow: "→",
  leftarrow: "←",
  Rightarrow: "⇒",
  ldots: "…",
  dots: "…",
  cdots: "…",
  sum: "∑",
  prod: "∏",
  sqrt: "√",
  infty: "∞",
  partial: "∂",
  alpha: "α",
  beta: "β",
  gamma: "γ",
  delta: "δ",
  Delta: "Δ",
  epsilon: "ε",
  theta: "θ",
  lambda: "λ",
  mu: "μ",
  pi: "π",
  rho: "ρ",
  sigma: "σ",
  Sigma: "Σ",
  tau: "τ",
  phi: "φ",
  omega: "ω",
};

// Commands that contribute nothing to a plain-text reading, so they vanish
// without leaving the space every other unknown command leaves behind.
const TEX_DROP = new Set([
  "left",
  "right",
  "displaystyle",
  "textstyle",
  "limits",
  "nolimits",
  "quad",
  "qquad",
  "big",
  "Big",
  "bigg",
  "Bigg",
]);

// A `$…$` run is only treated as math when it carries one of TeX's own
// markers. Prices are the reason: "it costs $5 and $7" is two dollar signs
// around ordinary prose, and stripping it as math would silently delete the
// currency from the preview.
const TEX_HINT = /[\\{}^_]/;

function texToText(tex: string): string {
  return (
    tex
      // \text{…} and its relatives wrap literal words — keep them, lose the
      // wrapper. Innermost-first, so \frac{\text{Cost}}{…} is unwrapped
      // before the fraction rule below needs brace-free arguments.
      .replace(
        /\\(?:text|textrm|textbf|textit|mathrm|mathbf|mathit|mathsf|mathtt|operatorname)\s*\{([^{}]*)\}/g,
        "$1",
      )
      // A fraction reads as a division on one line.
      .replace(/\\[dt]?frac\s*\{([^{}]*)\}\s*\{([^{}]*)\}/g, "$1/$2")
      // Escaped literals keep the character they escape.
      .replace(/\\([%$&#_{}])/g, "$1")
      // The line break and the spacing commands are whitespace.
      .replace(/\\\\|\\[,;:!]|\\ /g, " ")
      // Everything else: a known symbol, nothing, or a space — an unknown
      // command must not fuse the words either side of it.
      .replace(/\\([A-Za-z]+)/g, (_m, name: string) =>
        TEX_SYMBOLS[name] ?? (TEX_DROP.has(name) ? "" : " "),
      )
      // Grouping braces, alignment tabs and the sub/superscript markers are
      // pure notation: `1{,}000` is 1,000 and `x^2` is x2 to a one-line
      // reader.
      .replace(/[{}&^_]/g, "")
      .replace(/~/g, " ")
  );
}

// stripMarkdown renders a snippet as plain text for the one-line previews that
// have no markdown pipeline behind them (the project home's chat list). The
// server's preview is the raw last message, so a reply that opened with
// `**Proposed Memory:**` reached the UI with its asterisks intact — markup
// characters in a place that never renders markup.
//
// Deliberately a small, ordered set of rewrites rather than a parser: the goal
// is "no stray markup characters", not faithful rendering, and a preview is
// already collapsed to one line and truncated upstream. Fenced/inline code
// keeps its text and loses its backticks; links keep their label and lose the
// URL; emphasis, headings, quotes and list bullets lose their markers.
export function stripMarkdown(input: string): string {
  if (!input) return "";
  return (
    input
      // Images before links: ![alt](url) → alt (an image's label is all a
      // one-line preview can show of it).
      // The URL half allows BALANCED parens one level deep, which is what
      // Wikipedia-style links need: `[wiki](…/Foo_(bar))` stopped at the first
      // `)` and left a stray one in the preview.
      .replace(/!\[([^\]]*)\]\((?:[^()]|\([^()]*\))*\)/g, "$1")
      .replace(/\[([^\]]*)\]\((?:[^()]|\([^()]*\))*\)/g, "$1")
      // Fenced code: drop the fence lines (and any language tag) entirely.
      .replace(/```[^\n`]*\n?/g, "")
      .replace(/`+([^`]*)`+/g, "$1")
      // Math, after the code rules (a `$5` in backticks is a price, not TeX)
      // and before everything that pairs a backslash or an underscore.
      // Display math first, so `$$…$$` is never seen as two empty `$…$` runs.
      .replace(/\$\$([\s\S]*?)\$\$/g, (_m, body: string) => ` ${texToText(body)} `)
      .replace(/\$([^$\n]*)\$/g, (m, body: string) =>
        TEX_HINT.test(body) ? ` ${texToText(body)} ` : m,
      )
      // An UNCLOSED trailing run: the preview is clipped upstream, so the
      // closing delimiter is routinely the thing that got cut — which is how
      // the reported case (`$\text{CPM} = 1{,}000 \times \frac{…`) arrives.
      .replace(/\$([^$]*)$/, (m, body: string) =>
        TEX_HINT.test(body) ? ` ${texToText(body)} ` : m,
      )
      // Pipe tables. The alignment row carries no content at all, so it goes;
      // a data row keeps its cells and loses the pipes. A row is recognized by
      // a leading pipe or by two of them, which leaves a lone shell pipe
      // ("grep foo | wc -l") in ordinary prose alone.
      .replace(/^[ \t]*(?=[^\n]*\|)(?=[^\n]*-)[ \t|:-]+$/gm, "")
      .replace(/^[ \t]*(?:\|[^\n]*|[^\n|]*\|[^\n|]*\|[^\n]*)$/gm, (line) =>
        line
          .replace(/^[ \t]*\|/, "")
          .replace(/\|[ \t]*$/, "")
          .replace(/\|/g, " ")
          .trim(),
      )
      // Horizontal rules become nothing — BEFORE the bullet and emphasis
      // rules, which would otherwise eat a marker each and leave the rest
      // behind. Ordered after this rule, `***` came out as a bare `*` and
      // `- - -` as `- -`: stray markup in the one place that never renders it,
      // which is the whole reason this function exists. Escaped markers
      // (`\*`) are left to the emphasis rules below.
      .replace(/^\s{0,3}([-*_])[ \t]*(?:\1[ \t]*){2,}$/gm, "")
      // Leading block markers, per line: headings, quotes, list bullets.
      .replace(/^\s{0,3}#{1,6}\s+/gm, "")
      .replace(/^\s{0,3}>\s?/gm, "")
      .replace(/^\s{0,3}[-*+]\s+/gm, "")
      .replace(/^\s{0,3}\d+[.)]\s+/gm, "")
      // Backslash-escaped markers are placeholdered so the emphasis rules
      // below cannot pair them. CommonMark renders `\*not bold\*` as the
      // literal *not bold*; without this the escapes were consumed as if they
      // were emphasis and the text lost its asterisks AND kept its slashes.
      .replace(/\\([\\`*_{}[\]()#+\-.!~>])/g, (_m, ch: string) => `\uE000${ch.charCodeAt(0)};`)
      // Emphasis / strong / strikethrough markers around text. Asterisks and
      // underscores are handled separately because CommonMark treats them
      // differently, and so must this: an underscore INSIDE a word is not
      // emphasis, so `user_email_lookup` has to survive intact. The
      // word-boundary guards are what keep a snake_case identifier from
      // becoming useremaillookup in a preview.
      .replace(/\*\*\*(\S(?:[\s\S]*?\S)?)\*\*\*/g, "$1")
      .replace(/\*\*(\S(?:[\s\S]*?\S)?)\*\*/g, "$1")
      .replace(/\*(\S(?:[\s\S]*?\S)?)\*/g, "$1")
      // A leading capture group rather than a lookbehind: lookbehind is a
      // parse-time SyntaxError on older Safari, and a regex literal that fails
      // to parse takes the whole module down, not just this function.
      .replace(/(^|[^A-Za-z0-9_])___(\S(?:[\s\S]*?\S)?)___(?![A-Za-z0-9_])/g, "$1$2")
      .replace(/(^|[^A-Za-z0-9_])__(\S(?:[\s\S]*?\S)?)__(?![A-Za-z0-9_])/g, "$1$2")
      .replace(/(^|[^A-Za-z0-9_])_(\S(?:[\s\S]*?\S)?)_(?![A-Za-z0-9_])/g, "$1$2")
      .replace(/~~(\S(?:[\s\S]*?\S)?)~~/g, "$1")
      // Escaped markers come back as themselves, after every rule that could
      // have paired them.
      .replace(/\uE000(\d+);/g, (_m, code: string) => String.fromCharCode(Number(code)))
      // Whatever survives collapses to a single line.
      .replace(/\s+/g, " ")
      .trim()
  );
}
