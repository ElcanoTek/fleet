import { describe, expect, it } from "vitest";
import { PENDING_CONV_KEY, resolveWorkspaceHref } from "./workspaceHref";

const CONV = "fdf80072-b988-47fb-b3c0-11cb9cb1f0ba";

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
    const encoded = resolveWorkspaceHref("Comfluence%20Analysis%20Prompt.md", CONV);
    const raw = resolveWorkspaceHref("Comfluence Analysis Prompt.md", CONV);
    expect(encoded.href).toBe(
      `/api/conversations/${CONV}/workspace/Comfluence%20Analysis%20Prompt.md`,
    );
    expect(encoded.href).toBe(raw.href);
    expect(encoded.downloadFilename).toBe("Comfluence Analysis Prompt.md");
    expect(encoded.isWorkspaceFile).toBe(true);
  });

  it("handles a pre-encoded basename inside a sandbox: path", () => {
    // This is the exact shape that failed in production: a sandbox: URI whose
    // trailing filename had its spaces percent-encoded.
    expect(
      resolveWorkspaceHref(
        `sandbox:/opt/chat/workspace/${CONV}/Comfluence%20Analysis%20Prompt.md`,
        CONV,
      ),
    ).toEqual({
      href: `/api/conversations/${CONV}/workspace/Comfluence%20Analysis%20Prompt.md`,
      isWorkspaceFile: true,
      downloadFilename: "Comfluence Analysis Prompt.md",
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
});
