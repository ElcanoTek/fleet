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
      .replace(/!\[([^\]]*)\]\([^)]*\)/g, "$1")
      .replace(/\[([^\]]*)\]\([^)]*\)/g, "$1")
      // Fenced code: drop the fence lines (and any language tag) entirely.
      .replace(/```[^\n`]*\n?/g, "")
      .replace(/`+([^`]*)`+/g, "$1")
      // Leading block markers, per line: headings, quotes, list bullets.
      .replace(/^\s{0,3}#{1,6}\s+/gm, "")
      .replace(/^\s{0,3}>\s?/gm, "")
      .replace(/^\s{0,3}[-*+]\s+/gm, "")
      .replace(/^\s{0,3}\d+[.)]\s+/gm, "")
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
      // Horizontal rules become nothing.
      .replace(/^\s{0,3}([-*_])(?:\s*\1){2,}\s*$/gm, "")
      // Whatever survives collapses to a single line.
      .replace(/\s+/g, " ")
      .trim()
  );
}
