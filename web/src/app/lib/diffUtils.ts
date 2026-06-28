// Unified-diff detection and parsing for the chat transcript.
//
// Agents frequently emit file edits as unified diffs — sometimes inside a
// ```diff / ```patch fence, sometimes as a bare code block with no language
// tag. The chat renderer (AssistantContent's `pre` interceptor) uses
// isUnifiedDiff() to catch the untagged case and parseDiffLines() to classify
// each line so DiffBlock can colour it. Both functions are pure and string-only
// so they unit-test without React or a DOM.

// A single classified line of a unified diff. `content` is the displayable
// text with the leading marker (`+`, `-`, or a single context space) already
// stripped, so the gutter owns the marker and the body owns just the code.
// `raw` keeps the original line verbatim for parity checks in tests.
export type DiffLineType =
  | "file-header"
  | "hunk"
  | "addition"
  | "deletion"
  | "annotation"
  | "context";

export interface DiffLine {
  type: DiffLineType;
  content: string;
  raw: string;
}

// isUnifiedDiff returns true when the text looks like a unified diff even
// without a language tag: it must contain at least one file line (`--- ` or
// `+++ `) AND at least one hunk header (`@@ `). Requiring both keeps prose that
// merely uses a leading dash for a list item — or a horizontal rule of dashes —
// from being mistaken for a diff. The space after the markers is deliberate:
// `--- a/file` and `@@ -1,4 +1,5 @@` always carry it, while a Markdown thematic
// break (`---`) or a bullet (`- item`, never `--- `) does not.
export function isUnifiedDiff(text: string): boolean {
  const lines = text.split("\n");
  const hasFileLine = lines.some((l) => l.startsWith("--- ") || l.startsWith("+++ "));
  const hasHunk = lines.some((l) => l.startsWith("@@ "));
  return hasFileLine && hasHunk;
}

// parseDiffLines classifies every `\n`-split line of a unified diff so the
// renderer can apply per-line styling and an accessible +/- gutter (the colour
// is never the only signal). Classification order matters: the file-header
// (`+++`/`---`) and hunk (`@@`) checks run before the single-char `+`/`-`
// checks, so a `+++ b/file` line is a header rather than an addition.
export function parseDiffLines(raw: string): DiffLine[] {
  return raw.split("\n").map((line) => {
    if (line.startsWith("+++") || line.startsWith("---")) {
      return { type: "file-header", content: line, raw: line };
    }
    if (line.startsWith("@@")) {
      return { type: "hunk", content: line, raw: line };
    }
    if (line.startsWith("+")) {
      return { type: "addition", content: line.slice(1), raw: line };
    }
    if (line.startsWith("-")) {
      return { type: "deletion", content: line.slice(1), raw: line };
    }
    if (line.startsWith("\\")) {
      // "\ No newline at end of file" and similar git annotations.
      return { type: "annotation", content: line, raw: line };
    }
    // Context lines carry a single leading space in canonical unified diffs;
    // strip it so the body aligns with added/removed lines under the gutter.
    // Bare lines (some tools omit the space on blank context) pass through.
    return { type: "context", content: line.startsWith(" ") ? line.slice(1) : line, raw: line };
  });
}

// diffFilePath pulls the displayable path out of a file-header line so the
// renderer can show it as a pill. It drops the `+++ `/`--- ` marker and the
// common `a/` and `b/` prefixes git adds, and tolerates a trailing tab-
// separated timestamp (`--- a/file\t2024-…`). `/dev/null` is preserved as-is so
// added/deleted-file headers stay legible.
export function diffFilePath(headerLine: string): string {
  const withoutMarker = headerLine.replace(/^(\+\+\+|---)\s*/, "");
  const beforeTab = withoutMarker.split("\t")[0].trim();
  if (beforeTab === "/dev/null") return beforeTab;
  return beforeTab.replace(/^[ab]\//, "");
}
