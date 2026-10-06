import { describe, expect, it } from "vitest";
import {
  LOCKED_FILE_HREF,
  PENDING_CONV_KEY,
  linkSharedFiles,
  redactUnsharedFiles,
  teamFileDownloadName,
  workspaceFileRef,
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

  it("does not rewrite onto a malformed conversation id (fails closed)", () => {
    // The id gate (conversationApiUrl) refuses anything that is not one
    // path-safe token, so a malformed caller gets no workspace URL at all.
    for (const id of ["weird id/with slash", "../x", "http://evil", "%2e%2e"]) {
      expect(resolveWorkspaceHref("x.png", id)).toEqual({
        href: "x.png",
        isWorkspaceFile: false,
        downloadFilename: "",
      });
    }
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

  it("redacts a destination written with backslash escapes", () => {
    expect(redactUnsharedFiles("[report](my\\_file.csv)", IMAGE_PLACEHOLDER)).toBe(
      "my\\_file\\.csv (file not shared)",
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

// The team view (B19): shared outputs become live team-files links, every
// other workspace reference a locked name, uploads plain names. The public
// link keeps redactUnsharedFiles — pinned at the end of this block.
describe("redactUnsharedFiles — never withholds less than before", () => {
  it("still redacts escaped, commented and indented references", () => {
    // The public view may withhold MORE than renders, never less: these are
    // not links when rendered, but the redaction keeps treating them as
    // references, exactly as it always has.
    for (const md of [
      "See \\[x](secret.csv) here.",
      "Done <!-- [x](secret.csv) --> ok",
      "Intro.\n\n    [x](secret.csv)",
    ]) {
      expect(redactUnsharedFiles(md, "IMG")).toContain("(file not shared)");
    }
  });
});

describe("linkSharedFiles — the teammate's view of the owner's outputs", () => {
  const links = {
    shared: new Set(["daily_spend.png", "out/report final.xlsx", "data.csv"]),
    fileUrl: (path: string) =>
      `/api/conversations/${CONV}/team-files/${path
        .split("/")
        .map(encodeURIComponent)
        .join("/")}`,
  };
  const TEAM = `/api/conversations/${CONV}/team-files/`;
  const LOCK = `(${LOCKED_FILE_HREF})`;

  it("points a shared output's link and image at the team-files route", () => {
    expect(linkSharedFiles("[the data](data.csv)", links)).toBe(
      `[the data](${TEAM}data.csv)`,
    );
    expect(linkSharedFiles("![Spend](daily_spend.png)", links)).toBe(
      `![Spend](${TEAM}daily_spend.png)`,
    );
    // Nested folders and encoded names resolve to the same shared path.
    expect(linkSharedFiles("[r](out/report%20final.xlsx)", links)).toBe(
      `[r](${TEAM}out/report%20final.xlsx)`,
    );
    expect(
      linkSharedFiles(`[r](sandbox:/opt/chat/workspace/${CONV}/data.csv)`, links),
    ).toBe(`[r](${TEAM}data.csv)`);
  });

  it("leaves link-shaped text that renders as NO link exactly as written", () => {
    // The server's output discovery parses with CommonMark: none of these is
    // a link there (so none is an output), and none renders as one here.
    for (const md of [
      "See \\[x](secret.csv) here.",
      "See !\\[x](secret.png) here.",
      "Done <!-- [x](secret.csv) --> ok",
      "Intro.\n\n    [x](secret.csv)\n\nOutro.",
      "Intro.\n\n\t[x](data.csv)",
    ]) {
      expect(linkSharedFiles(md, links)).toBe(md);
    }
    // An escaped `!` leaves a LINK behind, rewritten like one.
    expect(linkSharedFiles("\\![d](data.csv)", links)).toBe(
      `\\![d](${TEAM}data.csv)`,
    );
    // Two backslashes escape each other: the bracket after them is live.
    expect(linkSharedFiles("\\\\[d](data.csv)", links)).toBe(
      `\\\\[d](${TEAM}data.csv)`,
    );
  });

  it("stays conservative where indentation is not a code block", () => {
    // A lazy continuation of a paragraph, and a list item's indented
    // paragraph, are prose — still rewritten.
    expect(linkSharedFiles("Intro.\n    [v](v.json)", links)).toBe(
      `Intro.\n    [v\\.json (not shared)]${LOCK}`,
    );
    expect(linkSharedFiles("- item\n\n    [v](v.json)", links)).toBe(
      `- item\n\n    [v\\.json (not shared)]${LOCK}`,
    );
  });

  it("rewrites a link the renderer un-codes from a `Label: code` line", () => {
    // AssistantContent turns `File: \`…\`` into a bold label and PLAIN text,
    // so the quoted link renders — and is rewritten like any other.
    expect(linkSharedFiles("File: `[d](data.csv)`", links)).toBe(
      `**File:** [d](${TEAM}data.csv)`,
    );
  });

  it("renders an unshared output as a locked name, never a live link", () => {
    expect(linkSharedFiles("[v1](exclusion_list_v1.json)", links)).toBe(
      `[exclusion\\_list\\_v1\\.json (not shared)]${LOCK}`,
    );
    // An unshared image is a locked name too: nothing is fetched.
    expect(linkSharedFiles("![c](chart.png)", links)).toBe(
      `[chart\\.png (not shared)]${LOCK}`,
    );
  });

  it("unescapes CommonMark backslash escapes in destinations before resolving", () => {
    // The Go parser that lists outputs drops these escapes (as the renderer
    // does), so the shared path is `my_file.csv` — not `my\_file.csv`.
    const esc = {
      shared: new Set(["my_file.csv", "out/a#1.csv", "ref_file.csv"]),
      fileUrl: links.fileUrl,
    };
    expect(linkSharedFiles("[report](my\\_file.csv)", esc)).toBe(
      `[report](${TEAM}my_file.csv)`,
    );
    expect(linkSharedFiles("![c](<out/a\\#1.csv>)", esc)).toBe(
      `![c](${TEAM}out/a%231.csv)`,
    );
    expect(linkSharedFiles("[r][x]\n\n[x]: ref\\_file.csv", esc)).toBe(
      `[r](${TEAM}ref_file.csv)\n`,
    );
    // An unshared escaped name locks by its real (unescaped) name.
    expect(linkSharedFiles("[v](held\\_v1.json)", esc)).toBe(
      `[held\\_v1\\.json (not shared)]${LOCK}`,
    );
  });

  it("names an upload plainly — uploads are never outputs, so never 'not shared'", () => {
    expect(linkSharedFiles("[brief](attachments/brief.pdf)", links)).toBe(
      "brief\\.pdf",
    );
  });

  it("rewrites both halves of a clickable thumbnail", () => {
    // shared thumb → shared full: a thumbnail link, both on the team route.
    expect(linkSharedFiles("[![t](daily_spend.png)](data.csv)", links)).toBe(
      `[![t](${TEAM}daily_spend.png)](${TEAM}data.csv)`,
    );
    // shared thumb → unshared full: the image stays, the link locks.
    expect(linkSharedFiles("[![t](daily_spend.png)](full.png)", links)).toBe(
      `![t](${TEAM}daily_spend.png) [full\\.png (not shared)]${LOCK}`,
    );
    // unshared thumb → shared full: the thumb locks, the full is linked by name.
    expect(linkSharedFiles("[![t](thumb.png)](data.csv)", links)).toBe(
      `[thumb\\.png (not shared)]${LOCK} [data\\.csv](${TEAM}data.csv)`,
    );
    // both unshared: two locked names; the same file: one.
    expect(linkSharedFiles("[![t](thumb.png)](full.png)", links)).toBe(
      `[thumb\\.png (not shared)]${LOCK} [full\\.png (not shared)]${LOCK}`,
    );
    expect(linkSharedFiles("[![t](chart.png)](chart.png)", links)).toBe(
      `[chart\\.png (not shared)]${LOCK}`,
    );
    // An external thumbnail linking to a workspace file, and the reverse.
    expect(linkSharedFiles("[![t](https://cdn.example/t.png)](full.png)", links)).toBe(
      `![t](https://cdn.example/t.png) [full\\.png (not shared)]${LOCK}`,
    );
    expect(linkSharedFiles("[![t](daily_spend.png)](https://example.com/x)", links)).toBe(
      `[![t](${TEAM}daily_spend.png)](https://example.com/x)`,
    );
    // Nothing in any of them still points at the owner's workspace.
    for (const md of [
      "[![t](daily_spend.png)](full.png)",
      "[![t](thumb.png)](data.csv)",
      "[![t](thumb.png)](full.png)",
    ]) {
      expect(linkSharedFiles(md, links)).not.toMatch(/\]\((?:full|thumb)\.png\)/);
    }
  });

  it("leaves the public redaction of a clickable thumbnail unchanged", () => {
    expect(redactUnsharedFiles("[![t](thumb.png)](full.png)", IMAGE_PLACEHOLDER)).toBe(
      "full\\.png (file not shared)",
    );
  });

  it("handles reference-style and bare routes by the same rules", () => {
    const md = ["See [the data][d] and [old][o].", "", "[d]: data.csv", "[o]: old.csv"].join("\n");
    expect(linkSharedFiles(md, links)).toBe(
      `See [the data](${TEAM}data.csv) and [old\\.csv (not shared)]${LOCK}.\n`,
    );
    expect(
      linkSharedFiles(`Saved to /api/conversations/${CONV}/workspace/data.csv.`, links),
    ).toBe(`Saved to [data\\.csv](${TEAM}data.csv).`);
  });

  it("resolves a duplicate reference label to its FIRST definition, like CommonMark", () => {
    // External first: the label is the external link; the later workspace
    // duplicate is inert and dropped, and the use is left alone.
    const extFirst = ["See [x].", "", "[x]: https://example.com/a", "[x]: data.csv"].join("\n");
    expect(linkSharedFiles(extFirst, links)).toBe(
      ["See [x].", "", "[x]: https://example.com/a"].join("\n"),
    );
    expect(redactUnsharedFiles(extFirst, IMAGE_PLACEHOLDER)).toBe(
      ["See [x].", "", "[x]: https://example.com/a"].join("\n"),
    );
    // Workspace first: the use is rewritten from it, and the later external
    // duplicate cannot become the label's definition once the first is gone.
    const wsFirst = ["See [x].", "", "[x]: old.csv", "[x]: https://example.com/a"].join("\n");
    expect(linkSharedFiles(wsFirst, links)).toBe(
      `See [old\\.csv (not shared)]${LOCK}.\n`,
    );
    expect(redactUnsharedFiles(wsFirst, IMAGE_PLACEHOLDER)).toBe(
      "See old\\.csv (file not shared).\n",
    );
    // Two workspace definitions: the first (shared) wins over the later one.
    const twoWs = ["See [x].", "", "[x]: data.csv", "[x]: old.csv"].join("\n");
    expect(linkSharedFiles(twoWs, links)).toBe(`See [x](${TEAM}data.csv).\n`);
    // External duplicates of an external label stay exactly as written.
    const twoExt = ["See [x].", "", "[x]: https://a.example", "[x]: https://b.example"].join("\n");
    expect(redactUnsharedFiles(twoExt, IMAGE_PLACEHOLDER)).toBe(twoExt);
    expect(linkSharedFiles(twoExt, links)).toBe(twoExt);
  });

  it("never mints a URL for a path outside the shared set, traversal included", () => {
    const out = linkSharedFiles(
      "[x](../data.csv) [y](%2e%2e/data.csv) [z](secret/data.csv)",
      links,
    );
    expect(out).not.toContain("team-files");
  });

  it("leaves external links, anchors and code alone", () => {
    const md = "[docs](https://example.com/a.csv) `[x](data.csv)`\n```\n[y](data.csv)\n```";
    expect(linkSharedFiles(md, links)).toBe(md);
  });

  it("does not change what a public link shows (files are never exposed there)", () => {
    expect(redactUnsharedFiles("[the data](data.csv)", "Image withheld.")).toBe(
      "data\\.csv (file not shared)",
    );
  });
});

describe("workspaceFileRef / teamFileDownloadName", () => {
  it("resolves the workspace-relative path a reference names", () => {
    expect(workspaceFileRef("out/a%20b.csv")).toEqual({ name: "a b.csv", path: "out/a b.csv" });
    expect(workspaceFileRef(`/api/conversations/${CONV}/workspace/x/y.png`)).toEqual({
      name: "y.png",
      path: "x/y.png",
    });
    expect(workspaceFileRef("https://example.com/y.png")).toBeNull();
  });

  it("names a team-files download and nothing else", () => {
    expect(teamFileDownloadName(`/api/conversations/${CONV}/team-files/out/a%20b.csv`)).toBe("a b.csv");
    expect(teamFileDownloadName(`/api/conversations/${CONV}/workspace/a.csv`)).toBeNull();
    expect(teamFileDownloadName("https://evil.example/api/conversations/x/team-files/a")).toBeNull();
  });
});
