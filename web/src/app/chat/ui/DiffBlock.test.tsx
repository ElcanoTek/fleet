import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { renderAssistantContent } from "./AssistantContent";
import { DiffBlock } from "./DiffBlock";

// Integration-flavored tests: drive the real ReactMarkdown pipeline the chat
// UI uses and assert that diff fences (and auto-detected bare diffs) render
// through DiffBlock with coloured +/- rows, a hunk header, a file pill, and the
// copy/apply toolbar — while non-diff code blocks stay on the plain <pre> path.

const FENCED_DIFF = [
  "```diff",
  "--- a/foo.ts",
  "+++ b/foo.ts",
  "@@ -1,3 +1,3 @@",
  " unchanged",
  "-const x = 1;",
  "+const x = 2;",
  "```",
].join("\n");

const BARE_DIFF = [
  "```",
  "--- a/foo.ts",
  "+++ b/foo.ts",
  "@@ -1,1 +1,1 @@",
  "-old",
  "+new",
  "```",
].join("\n");

function renderMarkdown(md: string) {
  return render(<>{renderAssistantContent(md, false, null)}</>);
}

describe("DiffBlock via renderAssistantContent", () => {
  it("renders a ```diff fence as a coloured diff block, not a plain <pre>", () => {
    const { container } = renderMarkdown(FENCED_DIFF);
    expect(screen.getByTestId("diff-block")).toBeInTheDocument();
    // No fallback plain-code wrapper for a diff fence.
    expect(container.querySelector(".assistant-markdown-pre")).toBeNull();
    // Addition / deletion rows are classified for colouring.
    expect(container.querySelector(".diff-line-addition")).not.toBeNull();
    expect(container.querySelector(".diff-line-deletion")).not.toBeNull();
    expect(container.querySelector(".diff-line-hunk")).not.toBeNull();
  });

  it("auto-detects a bare (untagged) unified diff and renders it as a diff block", () => {
    const { container } = renderMarkdown(BARE_DIFF);
    expect(screen.getByTestId("diff-block")).toBeInTheDocument();
    expect(container.querySelector(".diff-line-addition")).not.toBeNull();
    expect(container.querySelector(".diff-line-deletion")).not.toBeNull();
  });

  it("leaves a non-diff code block on the plain <pre> path", () => {
    const md = ["```ts", "const x = 1;", "```"].join("\n");
    const { container } = renderMarkdown(md);
    expect(screen.queryByTestId("diff-block")).toBeNull();
    expect(container.querySelector(".assistant-markdown-pre")).not.toBeNull();
  });
});

describe("DiffBlock", () => {
  it("shows the file path as a pill with the a/ b/ prefix stripped", () => {
    render(<DiffBlock raw={"--- a/src/foo.ts\n+++ b/src/foo.ts\n@@ -1 +1 @@\n-a\n+b"} />);
    const pills = document.querySelectorAll(".diff-file-pill");
    expect(pills.length).toBe(2);
    expect(pills[0].textContent).toBe("src/foo.ts");
  });

  it("exposes an accessible +/- gutter and screen-reader labels (not colour-only)", () => {
    render(<DiffBlock raw={"@@ -1 +1 @@\n-old\n+new"} />);
    // Gutter glyphs are present so additions/deletions read without colour.
    const gutters = Array.from(document.querySelectorAll(".diff-line-addition .diff-gutter, .diff-line-deletion .diff-gutter")).map(
      (g) => g.textContent,
    );
    expect(gutters).toContain("+");
    expect(gutters).toContain("-");
    // Screen-reader labels back up the colour for assistive tech.
    expect(screen.getByText(/added line/i)).toBeInTheDocument();
    expect(screen.getByText(/removed line/i)).toBeInTheDocument();
  });

  it("renders a disabled Apply button with a forward-compat tooltip", () => {
    render(<DiffBlock raw={"@@ -1 +1 @@\n-a\n+b"} />);
    const apply = screen.getByRole("button", { name: /apply/i });
    expect(apply).toBeDisabled();
    expect(apply).toHaveAttribute("title", "Apply is coming in a future release");
  });

  it("renders a Copy diff button", () => {
    render(<DiffBlock raw={"@@ -1 +1 @@\n-a\n+b"} />);
    expect(screen.getByRole("button", { name: /copy/i })).toBeInTheDocument();
  });
});
