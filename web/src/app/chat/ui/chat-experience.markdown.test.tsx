import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { autoFenceRawHtmlDocument, renderAssistantContent } from "./chat-experience";

const CONV = "fdf80072-b988-47fb-b3c0-11cb9cb1f0ba";

// Integration-flavored unit tests: drive the real ReactMarkdown pipeline
// the chat UI uses, then assert that <a> tags carry the workspace
// rewrite, the download affordance, and the visible-link class. These
// guard the regression reported when a relative .pptx link in the
// agent's reply rendered as plain text and downloaded nothing.

function renderMarkdown(md: string, conversationId: string | null = CONV) {
  return render(<>{renderAssistantContent(md, false, conversationId)}</>);
}

describe("renderAssistantContent — link rewriting", () => {
  it("rewrites a relative .pptx link to the workspace API and marks it as a download", () => {
    renderMarkdown("[Victoria_Test_Deck.pptx](Victoria_Test_Deck_g_dlgdopz39epsjlx.pptx)");
    const link = screen.getByRole("link", { name: /Victoria_Test_Deck\.pptx/ });
    expect(link).toHaveAttribute(
      "href",
      `/api/conversations/${CONV}/workspace/Victoria_Test_Deck_g_dlgdopz39epsjlx.pptx`,
    );
    // download carries the original (un-encoded) basename so the saved
    // file matches what the agent referenced.
    expect(link).toHaveAttribute("download", "Victoria_Test_Deck_g_dlgdopz39epsjlx.pptx");
    expect(link).toHaveClass("assistant-markdown-link");
    // Workspace downloads stay in the current tab; only external links
    // get target=_blank.
    expect(link).not.toHaveAttribute("target");
  });

  it("does not double-encode a spaced filename the model pre-encoded", () => {
    // Production regression ("the download link doesn't work"): the agent
    // emitted a markdown link whose spaces were already percent-encoded
    // (its own basename, or one parroted out of a sandbox: path). The old
    // rewrite re-encoded `%20` to `%2520`, so the browser fetched a file
    // that doesn't exist on disk and got a 404. Both the pre-encoded form
    // and the raw-space form must resolve to the same single-encoded href,
    // and the download attribute must be the real (decoded) filename.
    renderMarkdown("[Comfluence Analysis Prompt.md](Comfluence%20Analysis%20Prompt.md)");
    const link = screen.getByRole("link", { name: /Comfluence Analysis Prompt\.md/ });
    expect(link).toHaveAttribute(
      "href",
      `/api/conversations/${CONV}/workspace/Comfluence%20Analysis%20Prompt.md`,
    );
    expect(link).toHaveAttribute("download", "Comfluence Analysis Prompt.md");
    expect(link).toHaveClass("assistant-markdown-link");
    expect(link).not.toHaveAttribute("target");
  });

  it("rewrites a sandbox: path with a spaced, pre-encoded basename", () => {
    // The exact shape that failed: a hallucinated sandbox: URI carrying an
    // absolute workspace path whose trailing filename had encoded spaces.
    renderMarkdown(
      `[Comfluence Analysis Prompt.md](sandbox:/opt/chat/workspace/${CONV}/Comfluence%20Analysis%20Prompt.md)`,
    );
    const link = screen.getByRole("link", { name: /Comfluence Analysis Prompt\.md/ });
    expect(link).toHaveAttribute(
      "href",
      `/api/conversations/${CONV}/workspace/Comfluence%20Analysis%20Prompt.md`,
    );
    expect(link).toHaveAttribute("download", "Comfluence Analysis Prompt.md");
  });

  it("leaves absolute https URLs alone and opens them in a new tab", () => {
    const url = "https://gamma.app/docs/olcl2fs21agrkrl";
    renderMarkdown(`[View in Gamma](${url})`);
    const link = screen.getByRole("link", { name: /View in Gamma/ });
    expect(link).toHaveAttribute("href", url);
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveAttribute("rel", "noopener noreferrer");
    expect(link).not.toHaveAttribute("download");
    expect(link).toHaveClass("assistant-markdown-link");
  });

  it("renders relative paths verbatim when conversationId is null", () => {
    renderMarkdown("[deck.pptx](deck.pptx)", null);
    const link = screen.getByRole("link", { name: /deck\.pptx/ });
    // Without a conv id we can't construct an API path, so we leave
    // the agent's href untouched — the broken behaviour the new code
    // path is allowed to fall back to, but at least the link is still
    // styled as a link.
    expect(link).toHaveAttribute("href", "deck.pptx");
    expect(link).toHaveClass("assistant-markdown-link");
  });

  it("renders the link with visible styling that distinguishes it from plain text", () => {
    renderMarkdown("Click [here](https://example.com) please.");
    const link = screen.getByRole("link", { name: /here/ });
    // The class is what carries the color + underline rules in
    // globals.css; without it the link inherits body color and reads
    // as plain text.
    expect(link).toHaveClass("assistant-markdown-link");
  });
});

// Models occasionally paste a full HTML document directly into prose
// instead of wrapping it in a ```html fence. Without auto-fencing,
// ReactMarkdown silently drops the HTML and the user sees a gap with
// no preview. These guard the regression reported when Victoria's
// inventory-recommendation mockups rendered as nothing in chat.
describe("autoFenceRawHtmlDocument", () => {
  it("wraps an unfenced <!DOCTYPE html> document so InlineHtmlPreview can mount", () => {
    const raw = [
      "Here is the mockup:",
      "<!DOCTYPE html>",
      "<html><body><h1>Hi</h1></body></html>",
      "Let me know if you want changes.",
    ].join("\n");
    const fenced = autoFenceRawHtmlDocument(raw);
    expect(fenced).toContain("```html\n<!DOCTYPE html>");
    expect(fenced).toContain("</html>\n```");
    expect(fenced).toContain("Let me know if you want changes.");
  });

  it("wraps an unfenced <html> document with no doctype", () => {
    const raw = "<html>\n<body>x</body>\n</html>";
    expect(autoFenceRawHtmlDocument(raw)).toBe("```html\n<html>\n<body>x</body>\n</html>\n```");
  });

  it("leaves an already-fenced ```html block untouched", () => {
    const raw = "```html\n<!DOCTYPE html>\n<html></html>\n```";
    expect(autoFenceRawHtmlDocument(raw)).toBe(raw);
  });

  it("is a no-op on prose with no HTML document", () => {
    const raw = "Just markdown with a <span>inline</span> tag and **bold**.";
    expect(autoFenceRawHtmlDocument(raw)).toBe(raw);
  });

  it("keeps fence open mid-stream when </html> hasn't arrived yet", () => {
    const raw = "<!DOCTYPE html>\n<html><body>partial…";
    const fenced = autoFenceRawHtmlDocument(raw);
    expect(fenced.startsWith("```html\n<!DOCTYPE html>")).toBe(true);
    expect(fenced.endsWith("```")).toBe(true);
  });

  it("renders the wrapped HTML as a sandboxed iframe through ReactMarkdown", () => {
    const { container } = renderMarkdown(
      "Here is the mockup:\n<!DOCTYPE html>\n<html><body><h1>Hi</h1></body></html>",
    );
    const iframe = container.querySelector('iframe[title="HTML preview"]');
    expect(iframe).not.toBeNull();
    expect(iframe?.getAttribute("srcDoc")).toContain("<h1>Hi</h1>");
    // Also injects base tag to resolve relative paths
    expect(iframe?.getAttribute("srcDoc")).toContain(`<base href="/api/conversations/${CONV}/workspace/">`);
  });
});
