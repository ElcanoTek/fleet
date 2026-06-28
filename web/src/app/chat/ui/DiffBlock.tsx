"use client";

// DiffBlock renders a unified diff from an assistant message as a coloured,
// line-by-line diff view. The chat `pre` interceptor (AssistantContent) mounts
// it for ```diff / ```patch fences and for bare code blocks that match the
// unified-diff shape (see isUnifiedDiff). It deliberately does NOT change any
// non-diff rendering — plain code blocks keep the existing toolbar+<pre> path.
//
// Accessibility: colour is never the only signal. Every row carries a
// fixed-width gutter character (`+`, `-`, or a space) so additions/deletions
// remain distinguishable without colour, and added/removed rows expose a
// screen-reader label ("added line" / "removed line"). Styling lives in
// globals.css under the `.diff-*` class family and uses the shared theme tokens
// (--color-success / --color-danger / --color-accent) so it tracks both the
// dark and light themes rather than hardcoding hex.

import { CopyButton } from "./ChatChips";
import { diffFilePath, parseDiffLines, type DiffLine } from "@/app/lib/diffUtils";

// gutterChar returns the single character shown in the fixed-width gutter for a
// given line type. Additions/deletions get their marker; everything else gets a
// space so the bodies stay aligned under the gutter column.
function gutterChar(type: DiffLine["type"]): string {
  if (type === "addition") return "+";
  if (type === "deletion") return "-";
  return " ";
}

function FileHeaderRow({ line }: { line: DiffLine }) {
  const path = diffFilePath(line.content);
  return (
    <div className="diff-line diff-line-file-header" data-diff-line-type="file-header">
      <span className="diff-gutter" aria-hidden="true">
        {" "}
      </span>
      <span className="diff-line-body">
        <span className="diff-file-pill">{path}</span>
      </span>
    </div>
  );
}

export function DiffBlock({ raw }: { raw: string }) {
  // The fence interceptor passes the already-accumulated text; trim only the
  // single trailing newline the Markdown code child carries so we don't render
  // a spurious empty final row. Copy keeps the diff verbatim (raw text).
  const copyText = raw.replace(/\n$/, "");
  const lines = parseDiffLines(copyText);

  return (
    <div className="diff-block-wrapper" data-testid="diff-block">
      <div className="diff-block-toolbar assistant-markdown-pre-toolbar">
        <span className="assistant-markdown-pre-lang">diff</span>
        <span className="diff-block-actions">
          <button
            type="button"
            className="assistant-markdown-pre-copy diff-block-apply"
            disabled
            title="Apply is coming in a future release"
          >
            Apply
          </button>
          <CopyButton text={copyText} title="Copy diff to clipboard" variant="compact" />
        </span>
      </div>
      <div className="diff-block-body" role="group" aria-label="Unified diff">
        {lines.map((line, index) => {
          if (line.type === "file-header") {
            return <FileHeaderRow key={index} line={line} />;
          }
          const cls =
            line.type === "addition"
              ? "diff-line-addition"
              : line.type === "deletion"
                ? "diff-line-deletion"
                : line.type === "hunk"
                  ? "diff-line-hunk"
                  : line.type === "annotation"
                    ? "diff-line-annotation"
                    : "diff-line-context";
          // Screen readers should hear what each coloured row means; the
          // gutter glyph alone is ambiguous when read aloud.
          const srLabel =
            line.type === "addition"
              ? "added line"
              : line.type === "deletion"
                ? "removed line"
                : undefined;
          return (
            <div key={index} className={`diff-line ${cls}`} data-diff-line-type={line.type}>
              <span className="diff-gutter" aria-hidden="true">
                {gutterChar(line.type)}
              </span>
              <span className="diff-line-body">
                {srLabel ? <span className="sr-only">{srLabel}: </span> : null}
                {/* Render content verbatim, preserving leading whitespace; an
                    empty line still needs height, so emit a zero-width
                    placeholder when the body is empty. */}
                {line.content === "" ? "​" : line.content}
              </span>
            </div>
          );
        })}
      </div>
    </div>
  );
}
