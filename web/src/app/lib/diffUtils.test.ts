import { describe, expect, it } from "vitest";
import { diffFilePath, isUnifiedDiff, parseDiffLines } from "./diffUtils";

// Pure-string tests for the unified-diff detector and parser that back the
// chat DiffBlock renderer. These guard the heuristic that decides whether a
// bare (untagged) code block is a diff, and the per-line classification that
// drives the +add / -remove / @@hunk styling and the accessible gutter.

describe("isUnifiedDiff", () => {
  it("detects a canonical unified diff with file + hunk lines", () => {
    const diff = [
      "--- a/foo.ts",
      "+++ b/foo.ts",
      "@@ -1,3 +1,4 @@",
      " context",
      "-old",
      "+new",
    ].join("\n");
    expect(isUnifiedDiff(diff)).toBe(true);
  });

  it("requires BOTH a file line and a hunk header", () => {
    // Hunk header but no `--- `/`+++ ` file line.
    expect(isUnifiedDiff("@@ -1 +1 @@\n-old\n+new")).toBe(false);
    // File lines but no `@@ ` hunk.
    expect(isUnifiedDiff("--- a/foo\n+++ b/foo\njust text")).toBe(false);
  });

  it("does not mistake a markdown thematic break or bullet list for a diff", () => {
    // `---` thematic break and `- item` bullets must NOT trigger the heuristic:
    // the markers require a trailing space (`--- `, `@@ `) that prose lacks.
    expect(isUnifiedDiff("Intro\n\n---\n\n- one\n- two\n+ plus sign")).toBe(false);
  });
});

describe("parseDiffLines", () => {
  it("classifies each line type and strips the leading marker from the body", () => {
    const diff = [
      "--- a/foo.ts",
      "+++ b/foo.ts",
      "@@ -1,2 +1,2 @@ func ctx",
      " unchanged",
      "-removed",
      "+added",
      "\\ No newline at end of file",
    ].join("\n");
    const lines = parseDiffLines(diff);
    expect(lines.map((l) => l.type)).toEqual([
      "file-header",
      "file-header",
      "hunk",
      "context",
      "deletion",
      "addition",
      "annotation",
    ]);
    // Body strips the +/- marker (and a single context space) but keeps the
    // file-header / hunk / annotation lines whole.
    expect(lines[3].content).toBe("unchanged");
    expect(lines[4].content).toBe("removed");
    expect(lines[5].content).toBe("added");
    expect(lines[2].content).toBe("@@ -1,2 +1,2 @@ func ctx");
    expect(lines[6].content).toBe("\\ No newline at end of file");
  });

  it("does not treat a +++/--- file header as an addition/deletion", () => {
    const lines = parseDiffLines("+++ b/x\n--- a/x");
    expect(lines[0].type).toBe("file-header");
    expect(lines[1].type).toBe("file-header");
  });
});

describe("diffFilePath", () => {
  it("strips the marker and the a/ b/ prefix", () => {
    expect(diffFilePath("+++ b/src/app/foo.ts")).toBe("src/app/foo.ts");
    expect(diffFilePath("--- a/src/app/foo.ts")).toBe("src/app/foo.ts");
  });

  it("drops a trailing tab-separated timestamp", () => {
    expect(diffFilePath("--- a/foo.ts\t2026-01-01 00:00:00")).toBe("foo.ts");
  });

  it("preserves /dev/null for added/deleted-file headers", () => {
    expect(diffFilePath("--- /dev/null")).toBe("/dev/null");
  });
});
