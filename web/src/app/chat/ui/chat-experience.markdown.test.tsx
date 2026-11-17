import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
// Imported from AssistantContent directly: chat-experience no longer re-exports
// the markdown API — a static re-export would drag the lazy-loaded markdown
// module back into the initial /chat bundle.
import { autoFenceRawHtmlDocument, renderAssistantContent } from "./AssistantContent";

const CONV = "fdf80072-b988-47fb-b3c0-11cb9cb1f0ba";

// Integration-flavored unit tests: drive the real ReactMarkdown pipeline
// the chat UI uses, then assert that <a> tags carry the workspace
// rewrite, the download affordance, and the visible-link class. These
// guard the regression reported when a relative .pptx link in the
// agent's reply rendered as plain text and downloaded nothing.

function renderMarkdown(md: string, conversationId: string | null = CONV) {
  return render(<>{renderAssistantContent(md, false, conversationId)}</>);
}

describe("markdown renderer identity", () => {
  it("preserves a failed image placeholder when its parent rerenders", () => {
    const md = "![Generated chart](chart.png)";
    const { rerender } = renderMarkdown(md);
    fireEvent.error(screen.getByRole("img", { name: "Generated chart" }));
    const placeholder = screen.getByText(/couldn.t load image/);
    rerender(<>{renderAssistantContent(md, false, CONV)}</>);
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
    expect(screen.getByText(/couldn.t load image/)).toBe(placeholder);
  });

  it("preserves an HTML preview's source toggle while more text arrives", () => {
    const md = "```html\n<html><body>Preview</body></html>\n```";
    const { rerender } = renderMarkdown(md);
    fireEvent.click(screen.getByRole("button", { name: "Show source" }));
    rerender(<>{renderAssistantContent(`${md}\n\nMore explanation.`, false, CONV)}</>);
    expect(screen.getByRole("button", { name: "Show preview" })).toBeInTheDocument();
  });

  it("tries a changed image source and another conversation after an earlier failure", () => {
    const { rerender } = renderMarkdown("![Chart](old.png)");
    fireEvent.error(screen.getByRole("img", { name: "Chart" }));
    rerender(<>{renderAssistantContent("![Chart](new.png)", false, CONV)}</>);
    expect(screen.getByRole("img")).toHaveAttribute("src", `/api/conversations/${CONV}/workspace/new.png`);
    fireEvent.error(screen.getByRole("img"));
    rerender(<>{renderAssistantContent("![Chart](new.png)", false, "other-conversation")}</>);
    expect(screen.getByRole("img")).toHaveAttribute("src", "/api/conversations/other-conversation/workspace/new.png");
  });
});

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
    renderMarkdown("[Quarterly Analysis Prompt.md](Quarterly%20Analysis%20Prompt.md)");
    const link = screen.getByRole("link", { name: /Quarterly Analysis Prompt\.md/ });
    expect(link).toHaveAttribute(
      "href",
      `/api/conversations/${CONV}/workspace/Quarterly%20Analysis%20Prompt.md`,
    );
    expect(link).toHaveAttribute("download", "Quarterly Analysis Prompt.md");
    expect(link).toHaveClass("assistant-markdown-link");
    expect(link).not.toHaveAttribute("target");
  });

  it("rewrites a sandbox: path with a spaced, pre-encoded basename", () => {
    // The exact shape that failed: a hallucinated sandbox: URI carrying an
    // absolute workspace path whose trailing filename had encoded spaces.
    renderMarkdown(
      `[Quarterly Analysis Prompt.md](sandbox:/opt/chat/workspace/${CONV}/Quarterly%20Analysis%20Prompt.md)`,
    );
    const link = screen.getByRole("link", { name: /Quarterly Analysis Prompt\.md/ });
    expect(link).toHaveAttribute(
      "href",
      `/api/conversations/${CONV}/workspace/Quarterly%20Analysis%20Prompt.md`,
    );
    expect(link).toHaveAttribute("download", "Quarterly Analysis Prompt.md");
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
describe("renderAssistantContent — blocks", () => {
  it("gives lists, quotes and deep headings the classes their styles hang on", () => {
    const { container } = renderMarkdown(
      "- one\n- two\n\n1. first\n2. second\n\n> quoted\n\n#### Fourth level",
    );
    expect(container.querySelector("ul")).toHaveClass("assistant-markdown-ul");
    expect(container.querySelector("ol")).toHaveClass("assistant-markdown-ol");
    expect(container.querySelector("blockquote")).toHaveClass("assistant-markdown-quote");
    expect(container.querySelector("h4")).toHaveClass("assistant-markdown-h3");
  });

  // Steps separated by code blocks are separate lists; each keeps the number
  // it was written with instead of starting again at 1.
  it("keeps the number a list starts at", () => {
    const { container } = renderMarkdown(
      "1. Install\n\n```\nnpm i\n```\n\n2. Configure\n\n```\ninit\n```\n\n3. Run",
    );
    const lists = [...container.querySelectorAll("ol")];
    expect(lists.map((ol) => ol.start)).toEqual([1, 2, 3]);
    expect(lists[0]).not.toHaveAttribute("start");
  });

  it("widens the number gutter for a list that reaches three digits", () => {
    const { container } = renderMarkdown("98. a\n99. b\n100. c");
    expect(container.querySelector("ol")).toHaveAttribute("data-digits", "3");
    expect(renderMarkdown("1. a\n2. b").container.querySelector("ol")).not.toHaveAttribute("data-digits");
  });

  // A literal <br> is how models break a line inside a table cell.
  it("turns a <br> tag into a line break and leaves other tags as text", () => {
    const { container } = renderMarkdown(
      "| Plan | Includes |\n|---|---|\n| Team | 25 seats<br>Priority support<br/>SSO |\n\nPress <kbd>K</kbd> now.",
    );
    const cell = container.querySelectorAll("td")[1];
    expect(cell.querySelectorAll("br")).toHaveLength(2);
    expect(cell.textContent?.replace(/\n/g, "")).toBe("25 seatsPriority supportSSO");
    expect(container.querySelector("kbd")).toBeNull();
    expect(container.textContent).toContain("<kbd>K</kbd>");
  });

  // The whole rule for a literal <br>: a line break inside a table cell when
  // it has content on both sides, text everywhere else. Every row below is a case that an
  // earlier, cleverer rule got wrong in one direction or the other; a change
  // to the rule has to say what happens to each of them.
  describe("a literal <br>", () => {
    const shown = (md: string) => {
      const view = renderMarkdown(md);
      const text = view.container.textContent ?? "";
      const breaks = view.container.querySelectorAll("br").length;
      view.unmount();
      return { text, breaks };
    };

    it.each([
      // naming the tag
      "How do I use the <br> tag in an email?",
      "The tag for a line break is <br>",
      "The tag is:<br>",
      "Use <br>, not two spaces, and (<br>) too.",
      'He typed "<br>" and it vanished.',
      "Self-closing form: <br/>. Old form: <br>.",
      "the sequence <br><br> makes a blank line",
      "<br>s are everywhere",
      // HTML and code written without backticks
      "Fix this for me: <p>Hello<br>World</p>",
      "Why does <div><br></div> add a gap?",
      "Common void tags: <img>,<br>,<hr> and <input>.",
      "In HTML a<br>b puts b on a new line.",
      'echo "Hello<br>World";',
      "return <div>a<br/>b</div>;",
      "Here is my template:\n<span>Name<br>Title</span>",
      "In HTML you break a line with <br>\nand a paragraph with <p>.",
      // used as formatting outside a table: still shown, never guessed at
      "Acme Corp<br>\n123 Main St<br>\nSpringfield",
      "Thanks,<br>\nJohn<br>",
      "First<br><br>\nSecond",
      "Acme Corp<br>",
      "Name<br>Title<br>Company",
      "Name<br> Title",
      "Best regards,<br>\n<br>John Smith",
      "Options:<br>\n\n- one",
      "- Alpha<br>\n- Beta",
      "> Line one<br>\n> Line two",
      "## Title<br>Subtitle",
      // on a line of its own, and whole messages made of it
      "The tag you want is:\n\n<br>\n\nPut it at the end of the line.",
      "Before\n\n<br>\nAfter",
      "<br>",
      "<br>\n\n<br>",
      "<br><br>",
    ])("is shown as typed outside a table: %j", (md) => {
      const { text, breaks } = shown(md);
      const typed = (md.match(/<br\s*\/?>/gi) ?? []).length;
      expect((text.match(/<br\s*\/?>/gi) ?? []).length).toBe(typed);
      // Markdown's own hard breaks are the only <br> elements there can be.
      expect(breaks).toBe(0);
    });

    // Inside a table cell: [the cell as typed, line breaks, tags left showing].
    // A tag is a break only with content on both sides of it in the cell.
    const CELLS = [
      // Between two things: formatting.
      ["25 seats<br>Priority support<br/>SSO", 2, 0],
      ["a<BR>b", 1, 0],
      ["a <br> b", 1, 0],
      ["x<br><br>y", 2, 0],
      ["**x<br>y**", 1, 0],
      ["*a*<br>`code`", 1, 0],
      ["[link](https://example.com)<br>after", 1, 0],
      // Nothing on one side: the tag is what the cell is about.
      ["<br>", 0, 1],
      ["<br><br>", 0, 2],
      ["<br> or <br/>", 0, 2],
      ["<br> tag", 0, 1],
      ["Use <br>", 0, 1],
      ["text<br>", 0, 1],
      ["<br>a<br>b<br>", 1, 2],
      // In code it is never a tag at all.
      ["`<br>`", 0, 1],
      ["a `<br>` b", 0, 1],
      ["a `x<br>y` b", 0, 1],
      // Accepted: HTML pasted into a cell without backticks loses the tag it
      // has content around. Backticks keep it.
      ["<p>Hello<br>World</p>", 1, 0],
    ] as const;
    it.each(CELLS)("in a table cell: %j -> %i break(s), %i shown", (cell, breaks, shownTags) => {
      for (const md of [`| Plan |\n|---|\n| ${cell} |`, `| ${cell} | B |\n|---|---|\n| 1 | 2 |`]) {
        const view = renderMarkdown(md);
        const el = view.container.querySelector(md.startsWith("| Plan") ? "td" : "th");
        expect(el?.querySelectorAll("br")).toHaveLength(breaks);
        expect((el?.textContent?.match(/<br\s*\/?>/gi) ?? []).length).toBe(shownTags);
        view.unmount();
      }
    });

    it("decides each cell by itself", () => {
      const view = renderMarkdown("| Tag | Meaning |\n|---|---|\n| <br> | first<br>second |");
      const cells = [...view.container.querySelectorAll("td")];
      expect(cells[0].textContent).toBe("<br>");
      expect(cells[1].querySelectorAll("br")).toHaveLength(1);
    });

    it("leaves Markdown's own hard break alone", () => {
      const { text, breaks } = shown("Line one  \nLine two");
      expect(breaks).toBe(1);
      expect(text).toBe("Line one\nLine two");
    });
  });

  it("names a task-list checkbox and keeps it read-only", () => {
    renderMarkdown("- [x] shipped\n- [ ] pending");
    const boxes = screen.getAllByRole("checkbox", { name: "Task" });
    expect(boxes).toHaveLength(2);
    expect(boxes[0]).toBeChecked();
    expect(boxes[1]).not.toBeChecked();
    expect(boxes[0]).toBeDisabled();
  });

  // jsdom lays nothing out, so widths are stubbed: a block wider than its box
  // becomes a named keyboard stop; one that fits stays out of the Tab order.
  it("makes a code block or table a keyboard stop only while it scrolls sideways", () => {
    const client = vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(300);
    const scroll = vi.spyOn(HTMLElement.prototype, "scrollWidth", "get").mockReturnValue(900);
    try {
      const { container, unmount } = renderMarkdown("```\nlong line\n```\n\n| A | B |\n|---|---|\n| 1 | 2 |");
      expect(container.querySelector("pre")).toHaveAttribute("tabindex", "0");
      expect(screen.getByRole("group", { name: "Code, scrolls sideways" })).toBeInTheDocument();
      expect(screen.getByRole("group", { name: "Table, scrolls sideways" })).toBeInTheDocument();
      unmount();

      scroll.mockReturnValue(300);
      const fits = renderMarkdown("```\nshort\n```");
      expect(fits.container.querySelector("pre")).not.toHaveAttribute("tabindex");
      expect(screen.queryByRole("group")).toBeNull();
      // Never a landmark: several wide blocks would add several of one name.
      expect(screen.queryByRole("region")).toBeNull();
    } finally {
      client.mockRestore();
      scroll.mockRestore();
    }
  });
});

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
