import { describe, expect, it } from "vitest";
import {
  PENDING_CONV_KEY,
  redactUnsharedFiles,
  resolveTaskWorkspaceHref,
  resolveWorkspaceHref,
  unsharedFileName,
} from "./workspaceHref";

const CONV = "fdf80072-b988-47fb-b3c0-11cb9cb1f0ba";
const TASK = "11111111-1111-1111-1111-111111111111";

describe("resolveWorkspaceHref", () => {
  it("rewrites a relative file path to the workspace API", () => {
    const result = resolveWorkspaceHref("Victoria_Test_Deck_g_dlgdopz39epsjlx.pptx", CONV);
    expect(result.isWorkspaceFile).toBe(true);
    expect(result.href).toBe(
      `/api/conversations/${CONV}/workspace/Victoria_Test_Deck_g_dlgdopz39epsjlx.pptx`,
    );
    expect(result.downloadFilename).toBe("Victoria_Test_Deck_g_dlgdopz39epsjlx.pptx");
  });

  it("rewrites a relative subdirectory path and exposes only the basename", () => {
    const result = resolveWorkspaceHref("out/charts/spend.png", CONV);
    expect(result.isWorkspaceFile).toBe(true);
    expect(result.href).toBe(`/api/conversations/${CONV}/workspace/out/charts/spend.png`);
    expect(result.downloadFilename).toBe("spend.png");
  });

  it("percent-encodes filename segments with spaces and parens but keeps the raw basename", () => {
    const result = resolveWorkspaceHref("Q1 Report (Final).pptx", CONV);
    expect(result.isWorkspaceFile).toBe(true);
    expect(result.href).toBe(
      `/api/conversations/${CONV}/workspace/Q1%20Report%20(Final).pptx`,
    );
    // The download attribute uses the unencoded name so the saved file
    // is "Q1 Report (Final).pptx", not "Q1%20Report%20(Final).pptx".
    expect(result.downloadFilename).toBe("Q1 Report (Final).pptx");
  });

  it("leaves absolute https URLs alone", () => {
    const url = "https://assets.api.gamma.app/export/pptx/x/y/Deck.pptx";
    const result = resolveWorkspaceHref(url, CONV);
    expect(result.isWorkspaceFile).toBe(false);
    expect(result.href).toBe(url);
    expect(result.downloadFilename).toBe("");
  });

  it("leaves mailto: and data: URIs alone", () => {
    expect(resolveWorkspaceHref("mailto:a@b.com", CONV).isWorkspaceFile).toBe(false);
    expect(resolveWorkspaceHref("data:image/png;base64,AAAA", CONV).isWorkspaceFile).toBe(false);
  });

  it("leaves protocol-relative and site-absolute paths alone", () => {
    expect(resolveWorkspaceHref("//cdn.example/x.png", CONV).isWorkspaceFile).toBe(false);
    expect(resolveWorkspaceHref("/api/whatever", CONV).isWorkspaceFile).toBe(false);
  });

  it("leaves in-page anchors and query strings alone", () => {
    expect(resolveWorkspaceHref("#section-2", CONV).isWorkspaceFile).toBe(false);
    expect(resolveWorkspaceHref("?tab=details", CONV).isWorkspaceFile).toBe(false);
  });

  it("strips hallucinated sandbox:/ schemes and absolute paths", () => {
    // LLM outputs `sandbox:/opt/chat/workspace/<convId>/file.xlsx`
    expect(resolveWorkspaceHref(`sandbox:/opt/chat/workspace/${CONV}/file.xlsx`, CONV)).toEqual({
      href: `/api/conversations/${CONV}/workspace/file.xlsx`,
      isWorkspaceFile: true,
      downloadFilename: "file.xlsx",
    });

    // LLM outputs `sandbox:/opt/chat/workspace/some-other-uuid/file.xlsx`
    expect(resolveWorkspaceHref("sandbox:/opt/chat/workspace/12345678-1234-1234-1234-123456789abc/file.xlsx", CONV)).toEqual({
      href: `/api/conversations/${CONV}/workspace/file.xlsx`,
      isWorkspaceFile: true,
      downloadFilename: "file.xlsx",
    });

    // LLM outputs `sandbox:file.xlsx`
    expect(resolveWorkspaceHref("sandbox:file.xlsx", CONV)).toEqual({
      href: `/api/conversations/${CONV}/workspace/file.xlsx`,
      isWorkspaceFile: true,
      downloadFilename: "file.xlsx",
    });

    // LLM outputs `sandbox://opt/chat/workspace/foo/bar.txt`
    expect(resolveWorkspaceHref("sandbox://opt/chat/workspace/foo/bar.txt", CONV)).toEqual({
      href: `/api/conversations/${CONV}/workspace/foo/bar.txt`,
      isWorkspaceFile: true,
      downloadFilename: "bar.txt",
    });

    // LLM outputs `/opt/chat/workspace/foo.xlsx`
    expect(resolveWorkspaceHref("/opt/chat/workspace/foo.xlsx", CONV)).toEqual({
      href: `/api/conversations/${CONV}/workspace/foo.xlsx`,
      isWorkspaceFile: true,
      downloadFilename: "foo.xlsx",
    });
  });

  it("does not double-encode an already percent-encoded filename", () => {
    // Regression: the model emits a markdown link whose spaces are already
    // percent-encoded (its own basename, or one parroted out of a sandbox:
    // path). Re-encoding `%20` to `%2520` made the workspace fetch 404 on a
    // file that exists. A pre-encoded and a raw filename must resolve to the
    // same single-encoded href, and the download attribute must be the real
    // (decoded) name.
    const encoded = resolveWorkspaceHref("Quarterly%20Analysis%20Prompt.md", CONV);
    const raw = resolveWorkspaceHref("Quarterly Analysis Prompt.md", CONV);
    expect(encoded.href).toBe(
      `/api/conversations/${CONV}/workspace/Quarterly%20Analysis%20Prompt.md`,
    );
    expect(encoded.href).toBe(raw.href);
    expect(encoded.downloadFilename).toBe("Quarterly Analysis Prompt.md");
    expect(encoded.isWorkspaceFile).toBe(true);
  });

  it("handles a pre-encoded basename inside a sandbox: path", () => {
    // This is the exact shape that failed in production: a sandbox: URI whose
    // trailing filename had its spaces percent-encoded.
    expect(
      resolveWorkspaceHref(
        `sandbox:/opt/chat/workspace/${CONV}/Quarterly%20Analysis%20Prompt.md`,
        CONV,
      ),
    ).toEqual({
      href: `/api/conversations/${CONV}/workspace/Quarterly%20Analysis%20Prompt.md`,
      isWorkspaceFile: true,
      downloadFilename: "Quarterly Analysis Prompt.md",
    });
  });

  it("keeps a literal percent in a filename that is not a valid escape", () => {
    // `%of` is not a valid percent-escape; decodeURIComponent throws, so we
    // fall back to the raw segment and encode the literal `%`.
    const r = resolveWorkspaceHref("50%off-report.csv", CONV);
    expect(r.href).toBe(`/api/conversations/${CONV}/workspace/50%25off-report.csv`);
    expect(r.downloadFilename).toBe("50%off-report.csv");
  });

  it("returns the raw href when conversationId is null or pending", () => {
    expect(resolveWorkspaceHref("file.pptx", null)).toEqual({
      href: "file.pptx",
      isWorkspaceFile: false,
      downloadFilename: "",
    });
    expect(resolveWorkspaceHref("file.pptx", PENDING_CONV_KEY)).toEqual({
      href: "file.pptx",
      isWorkspaceFile: false,
      downloadFilename: "",
    });
  });

  it("returns an empty href for empty / non-string input", () => {
    expect(resolveWorkspaceHref("", CONV)).toEqual({
      href: "",
      isWorkspaceFile: false,
      downloadFilename: "",
    });
    expect(resolveWorkspaceHref(undefined, CONV)).toEqual({
      href: "",
      isWorkspaceFile: false,
      downloadFilename: "",
    });
    expect(resolveWorkspaceHref(null, CONV)).toEqual({
      href: "",
      isWorkspaceFile: false,
      downloadFilename: "",
    });
  });

  it("URL-encodes the conversation id to defend against malformed callers", () => {
    const result = resolveWorkspaceHref("x.png", "weird id/with slash");
    expect(result.href.startsWith("/api/conversations/weird%20id%2Fwith%20slash/workspace/")).toBe(true);
  });

  it("refuses . / .. / encoded-dot segments instead of rewriting them (#1113)", () => {
    const passthrough = (raw: string) => ({
      href: raw,
      isWorkspaceFile: false,
      downloadFilename: "",
    });

    // The motivating prompt-injection: a markdown link that would become
    // /api/conversations/<id>/workspace/../../auth/elcano-login and then
    // a cookie-bearing GET of /api/auth/elcano-login after the browser
    // normalizes dot-segments.
    expect(resolveWorkspaceHref("../../auth/elcano-login", CONV)).toEqual(
      passthrough("../../auth/elcano-login"),
    );
    expect(resolveWorkspaceHref("foo/../secret", CONV)).toEqual(passthrough("foo/../secret"));
    expect(resolveWorkspaceHref("./file.xlsx", CONV)).toEqual(passthrough("./file.xlsx"));
    expect(resolveWorkspaceHref("out/./chart.png", CONV)).toEqual(passthrough("out/./chart.png"));
    expect(resolveWorkspaceHref(".", CONV)).toEqual(passthrough("."));
    expect(resolveWorkspaceHref("..", CONV)).toEqual(passthrough(".."));

    // Single-encoded (decodeURIComponent("%2e%2e") === "..").
    expect(resolveWorkspaceHref("%2e%2e/%2e%2e/auth/elcano-login", CONV)).toEqual(
      passthrough("%2e%2e/%2e%2e/auth/elcano-login"),
    );
    expect(resolveWorkspaceHref("%2E%2E/secret", CONV)).toEqual(passthrough("%2E%2E/secret"));
    expect(resolveWorkspaceHref("%2e/file.xlsx", CONV)).toEqual(passthrough("%2e/file.xlsx"));

    // Double-encoded: one decode yields "%2e%2e", not "..".
    expect(resolveWorkspaceHref("%252e%252e/secret", CONV)).toEqual(
      passthrough("%252e%252e/secret"),
    );
    expect(resolveWorkspaceHref("%252E%252E/%252e/file.xlsx", CONV)).toEqual(
      passthrough("%252E%252E/%252e/file.xlsx"),
    );

    // A real workspace filename that merely starts with a dot is still rewritten.
    const hidden = resolveWorkspaceHref(".gitignore", CONV);
    expect(hidden).toEqual({
      href: `/api/conversations/${CONV}/workspace/.gitignore`,
      isWorkspaceFile: true,
      downloadFilename: ".gitignore",
    });
    const dotted = resolveWorkspaceHref("out/.cache/chart.png", CONV);
    expect(dotted).toEqual({
      href: `/api/conversations/${CONV}/workspace/out/.cache/chart.png`,
      isWorkspaceFile: true,
      downloadFilename: "chart.png",
    });
  });
});

describe("resolveTaskWorkspaceHref", () => {
  it("rewrites a relative image path to the task workspace API", () => {
    const result = resolveTaskWorkspaceHref("weekly-infographic.png", TASK);
    expect(result.isWorkspaceFile).toBe(true);
    expect(result.href).toBe(`/api/orchestrator/tasks/${TASK}/workspace/weekly-infographic.png`);
    expect(result.downloadFilename).toBe("weekly-infographic.png");
  });

  it("rewrites a relative subdirectory path and exposes only the basename", () => {
    const result = resolveTaskWorkspaceHref("out/charts/spend.png", TASK);
    expect(result.isWorkspaceFile).toBe(true);
    expect(result.href).toBe(`/api/orchestrator/tasks/${TASK}/workspace/out/charts/spend.png`);
    expect(result.downloadFilename).toBe("spend.png");
  });

  it("percent-encodes filename segments with spaces but keeps the raw basename", () => {
    const result = resolveTaskWorkspaceHref("Q1 Report (Final).png", TASK);
    expect(result.isWorkspaceFile).toBe(true);
    expect(result.href).toBe(`/api/orchestrator/tasks/${TASK}/workspace/Q1%20Report%20(Final).png`);
    expect(result.downloadFilename).toBe("Q1 Report (Final).png");
  });

  it("does not double-encode an already percent-encoded filename", () => {
    const encoded = resolveTaskWorkspaceHref("Weekly%20Infographic.png", TASK);
    const raw = resolveTaskWorkspaceHref("Weekly Infographic.png", TASK);
    expect(encoded.href).toBe(`/api/orchestrator/tasks/${TASK}/workspace/Weekly%20Infographic.png`);
    expect(encoded.href).toBe(raw.href);
    expect(encoded.downloadFilename).toBe("Weekly Infographic.png");
  });

  it("strips a hallucinated sandbox:/opt/chat/workspace path to a task-scoped href", () => {
    expect(
      resolveTaskWorkspaceHref(`sandbox:/opt/chat/workspace/${TASK}/chart.png`, TASK),
    ).toEqual({
      href: `/api/orchestrator/tasks/${TASK}/workspace/chart.png`,
      isWorkspaceFile: true,
      downloadFilename: "chart.png",
    });
  });

  it("leaves absolute https URLs alone (no SSRF / remote-fetch rewrite)", () => {
    const url = "https://evil.example/track.png";
    const result = resolveTaskWorkspaceHref(url, TASK);
    expect(result.isWorkspaceFile).toBe(false);
    expect(result.href).toBe(url);
    expect(result.downloadFilename).toBe("");
  });

  it("leaves data: URIs, protocol-relative, and site-absolute paths alone", () => {
    expect(resolveTaskWorkspaceHref("data:image/png;base64,AAAA", TASK).isWorkspaceFile).toBe(false);
    expect(resolveTaskWorkspaceHref("//cdn.example/x.png", TASK).isWorkspaceFile).toBe(false);
    expect(resolveTaskWorkspaceHref("/api/whatever", TASK).isWorkspaceFile).toBe(false);
  });

  it("returns the raw href when the task id is null", () => {
    expect(resolveTaskWorkspaceHref("chart.png", null)).toEqual({
      href: "chart.png",
      isWorkspaceFile: false,
      downloadFilename: "",
    });
  });

  it("returns an empty href for empty / non-string input", () => {
    expect(resolveTaskWorkspaceHref("", TASK)).toEqual({
      href: "",
      isWorkspaceFile: false,
      downloadFilename: "",
    });
    expect(resolveTaskWorkspaceHref(undefined, TASK)).toEqual({
      href: "",
      isWorkspaceFile: false,
      downloadFilename: "",
    });
  });

  it("URL-encodes the task id to defend against malformed callers", () => {
    const result = resolveTaskWorkspaceHref("x.png", "weird id/with slash");
    expect(
      result.href.startsWith("/api/orchestrator/tasks/weird%20id%2Fwith%20slash/workspace/"),
    ).toBe(true);
  });

  it("refuses . / .. / encoded-dot segments instead of rewriting them (#1113)", () => {
    const passthrough = (raw: string) => ({
      href: raw,
      isWorkspaceFile: false,
      downloadFilename: "",
    });
    expect(resolveTaskWorkspaceHref("../../auth/elcano-login", TASK)).toEqual(
      passthrough("../../auth/elcano-login"),
    );
    expect(resolveTaskWorkspaceHref("%2e%2e/secret", TASK)).toEqual(passthrough("%2e%2e/secret"));
    expect(resolveTaskWorkspaceHref("%252e%252e/secret", TASK)).toEqual(
      passthrough("%252e%252e/secret"),
    );
    expect(resolveTaskWorkspaceHref("./chart.png", TASK)).toEqual(passthrough("./chart.png"));
    const hidden = resolveTaskWorkspaceHref(".gitignore", TASK);
    expect(hidden.isWorkspaceFile).toBe(true);
    expect(hidden.href).toBe(`/api/orchestrator/tasks/${TASK}/workspace/.gitignore`);
  });
});

// ── the read-only views' inverse question (#9) ─────────────────────────────
//
// A teammate's team view and a public share link expose the transcript ONLY:
// the files it names stay behind the owner-scoped workspace route, which
// answers neither reader. These cover the two halves of rendering that
// honestly — recognising such an href, and rewriting the markdown that carries
// it into plain text.

const IMAGE_PLACEHOLDER = "Image not shared with team views.";

describe("unsharedFileName", () => {
  it("names the file behind an agent-emitted relative path", () => {
    expect(unsharedFileName("daily_spend_by_channel.png")).toBe(
      "daily_spend_by_channel.png",
    );
    expect(unsharedFileName("out/charts/spend.png")).toBe("spend.png");
    expect(unsharedFileName("Q1%20Report.pptx")).toBe("Q1 Report.pptx");
  });

  it("names the file behind a hallucinated sandbox path", () => {
    expect(unsharedFileName(`sandbox:/opt/chat/workspace/${CONV}/file.xlsx`)).toBe(
      "file.xlsx",
    );
    expect(unsharedFileName("/opt/chat/workspace/chart.png")).toBe("chart.png");
  });

  it("names the file behind an already-resolved workspace route", () => {
    expect(unsharedFileName(`/api/conversations/${CONV}/workspace/spend.png`)).toBe(
      "spend.png",
    );
    expect(unsharedFileName(`/api/conversations/${CONV}/workspace/a%20b.csv`)).toBe(
      "a b.csv",
    );
    expect(unsharedFileName(`/api/orchestrator/tasks/${TASK}/workspace/weekly.png`)).toBe(
      "weekly.png",
    );
  });

  it("never claims a fully-qualified URL, whatever its host", () => {
    // Two reasons, both deliberate. The route shape is not ours to claim on
    // someone else's host — that page is one the reader can simply open, and
    // "file not shared" would be a false statement. And deciding by origin
    // cannot be made deterministic: `location` does not exist during the
    // server pass, so the same href would render as a live link on the server
    // and as plain text after hydration — a mismatch, and in the prerendered
    // public share view a dead owner-scoped link visible until hydration.
    //
    // Nothing real is lost: resolveWorkspaceHref only ever produces
    // root-relative routes.
    expect(
      unsharedFileName(
        `https://example.com/api/conversations/${CONV}/workspace/chart.png`,
      ),
    ).toBeNull();
    expect(
      unsharedFileName(
        `${location.origin}/api/conversations/${CONV}/workspace/a%20b.csv`,
      ),
    ).toBeNull();
  });

  it("leaves every href a read-only reader can still follow", () => {
    expect(unsharedFileName("https://example.com/docs")).toBeNull();
    expect(unsharedFileName("http://example.com/api/conversations/x")).toBeNull();
    expect(unsharedFileName("mailto:a@b.com")).toBeNull();
    expect(unsharedFileName("data:image/png;base64,AAAA")).toBeNull();
    expect(unsharedFileName("//cdn.example/x.png")).toBeNull();
    expect(unsharedFileName("/settings")).toBeNull();
    expect(unsharedFileName("#section-2")).toBeNull();
    expect(unsharedFileName("")).toBeNull();
    expect(unsharedFileName(null)).toBeNull();
  });

  it("refuses traversal instead of naming a file for it (#1113)", () => {
    expect(unsharedFileName("../../auth/elcano-login")).toBeNull();
    expect(unsharedFileName("%252e%252e/secret")).toBeNull();
  });
});

describe("redactUnsharedFiles", () => {
  it("turns a markdown link to a workspace file into plain marked text", () => {
    expect(
      redactUnsharedFiles(
        "Full data: [daily_spend_by_channel.csv](daily_spend_by_channel.csv)",
        IMAGE_PLACEHOLDER,
      ),
    ).toBe(
      "Full data: daily\\_spend\\_by\\_channel\\.csv (file not shared)",
    );
  });

  it("replaces an embedded workspace image with the caller's placeholder", () => {
    expect(
      redactUnsharedFiles("![Daily spend](daily_spend_by_channel.png)", IMAGE_PLACEHOLDER),
    ).toBe(IMAGE_PLACEHOLDER);
    // An image wrapped in a link to the same file — the "click the chart to
    // download it" shape — leaves neither an image nor an anchor behind.
    expect(
      redactUnsharedFiles("[![Chart](chart.png)](chart.png)", IMAGE_PLACEHOLDER),
    ).toBe("chart\\.png (file not shared)");
  });

  it("leaves links and images a reader can still resolve alone", () => {
    const md =
      "See [the docs](https://example.com/attribution) and ![logo](https://cdn.example/l.png) or ![inline](data:image/png;base64,AAAA).";
    expect(redactUnsharedFiles(md, IMAGE_PLACEHOLDER)).toBe(md);
  });

  it("marks a bare workspace route pasted into prose, keeping the sentence", () => {
    expect(
      redactUnsharedFiles(
        `Saved to /api/conversations/${CONV}/workspace/spend.png.`,
        IMAGE_PLACEHOLDER,
      ),
    ).toBe("Saved to spend\\.png (file not shared).");
    expect(
      redactUnsharedFiles(
        `It is at sandbox:/opt/chat/workspace/${CONV}/deck.pptx`,
        IMAGE_PLACEHOLDER,
      ),
    ).toBe("It is at deck\\.pptx (file not shared)");
  });

  it("does not touch a bare filename in prose", () => {
    const md = "I wrote the numbers to spend.png and moved on.";
    expect(redactUnsharedFiles(md, IMAGE_PLACEHOLDER)).toBe(md);
  });

  it("handles the reference form, definition and all", () => {
    const md = [
      "The [chart][1] and the [table][tbl].",
      "",
      "[1]: chart.png",
      "[tbl]: https://example.com/table",
    ].join("\n");
    expect(redactUnsharedFiles(md, IMAGE_PLACEHOLDER)).toBe(
      ["The chart\\.png (file not shared) and the [table][tbl].", "", "[tbl]: https://example.com/table"].join("\n"),
    );
  });

  it("leaves fenced blocks and inline code verbatim — quoted source is not a link", () => {
    const md = [
      "Run this:",
      "",
      "```python",
      "plt.savefig('daily_spend_by_channel.png')",
      "```",
      "",
      "Then read `[spend](spend.csv)` back.",
    ].join("\n");
    expect(redactUnsharedFiles(md, IMAGE_PLACEHOLDER)).toBe(md);
  });

  it("closes a fence only on a marker at least as long as the opener", () => {
    // Documentation quoting a ``` block inside a ```` one. Comparing only the
    // marker character closed the outer block on the inner fence: everything
    // after it was treated as prose (so the sample's own filename got
    // rewritten) and the real closing fence opened a phantom block (so the
    // genuine link below escaped redaction) — wrong in both directions.
    const md = [
      "How to show a chart:",
      "",
      "````markdown",
      "```python",
      "plt.savefig('daily_spend_by_channel.png')",
      "```",
      "See [chart](daily_spend_by_channel.png) afterwards.",
      "````",
      "",
      "Here is the real one: [chart](daily_spend_by_channel.png)",
    ].join("\n");
    const out = redactUnsharedFiles(md, IMAGE_PLACEHOLDER);
    // Everything inside the four-backtick block is verbatim, both fences and
    // the sample link included.
    expect(out).toContain("```python");
    expect(out).toContain("See [chart](daily_spend_by_channel.png) afterwards.");
    // …and the real link outside it is still withheld.
    expect(out).toContain(
      "Here is the real one: daily\\_spend\\_by\\_channel\\.png (file not shared)",
    );
  });

  it("is a no-op on text that names no files", () => {
    const md = "Revenue rose 12% in Q3 — mostly paid search.";
    expect(redactUnsharedFiles(md, IMAGE_PLACEHOLDER)).toBe(md);
    expect(redactUnsharedFiles("", IMAGE_PLACEHOLDER)).toBe("");
  });
});
