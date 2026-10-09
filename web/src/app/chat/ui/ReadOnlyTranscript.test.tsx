import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { renderAssistantContent } from "./AssistantContent";
import { ReadOnlyTranscript, toBubbles, type ReadOnlyAudience } from "./ReadOnlyTranscript";
import { ReadOnlyFilesContext, WithheldFilesContext } from "./LockedFiles";

// The read-only renderer both doors onto someone else's conversation share:
// a teammate's team view and a public share link. What it must NOT do is
// promise a file neither reader can fetch — attachments and generated files
// stay behind the owner-scoped workspace route (docs/TEAM-SHARING.md), so a
// live link to one is a 404 dressed as a download.
//
// Driven through the REAL markdown pipeline (renderAssistantContent, exactly
// what the public share page passes), because the guarantee being tested is
// about the DOM that comes out: no <a>, no <img>.

afterEach(cleanup);

// One fixture carrying the three cases from the QA finding: a markdown link to
// a workspace file, an embedded generated image, and an ordinary external link
// that must stay clickable.
const TRANSCRIPT = [
  {
    id: 1,
    role: "user",
    type: "text",
    content: { text: "How did spend break down by channel?" },
  },
  {
    id: 2,
    role: "assistant",
    type: "text",
    content: {
      text: [
        "Here is the breakdown.",
        "",
        "![Daily spend by channel](daily_spend_by_channel.png)",
        "",
        "Full data: [daily_spend_by_channel.csv](daily_spend_by_channel.csv)",
        "",
        "Method: [the attribution docs](https://example.com/attribution).",
      ].join("\n"),
    },
  },
];

function renderTranscript(audience: ReadOnlyAudience = "team") {
  return render(
    <ReadOnlyTranscript
      bubbles={toBubbles(TRANSCRIPT)}
      audience={audience}
      renderAssistant={(text) => renderAssistantContent(text, false, null)}
    />,
  );
}

describe("ReadOnlyTranscript — files the reader cannot fetch", () => {
  it("renders a workspace file reference as plain text, with no anchor at all", () => {
    const { container } = renderTranscript();

    // The filename is still readable — the reader can ask the owner for it by
    // name — but nothing is clickable, because a disabled link would still be
    // a dead promise.
    expect(
      screen.getByText(/daily_spend_by_channel\.csv \(file not shared\)/),
    ).toBeInTheDocument();
    expect(
      container.querySelector('a[href*="daily_spend_by_channel.csv"]'),
    ).toBeNull();
    expect(
      screen.queryByRole("link", { name: /daily_spend_by_channel\.csv/ }),
    ).toBeNull();
  });

  it("withholds an embedded image and says so — never as a load error", () => {
    const { container } = renderTranscript();

    expect(screen.getByText("Image not shared with team views.")).toBeInTheDocument();
    // No <img> is mounted, so nothing is requested and nothing can fail:
    // "couldn't load image" described an error when nothing had failed.
    expect(container.querySelector("img")).toBeNull();
    expect(screen.queryByText(/couldn’t load image/i)).toBeNull();
    expect(screen.queryByText(/daily_spend_by_channel\.png/)).toBeNull();
  });

  it("keeps an ordinary external link clickable", () => {
    renderTranscript();

    const link = screen.getByRole("link", { name: "the attribution docs" });
    expect(link).toHaveAttribute("href", "https://example.com/attribution");
    expect(link).toHaveAttribute("target", "_blank");
    // …and it is the ONLY link in the transcript.
    expect(screen.getAllByRole("link")).toHaveLength(1);
  });

  it("words the image placeholder for the reader it actually has", () => {
    renderTranscript("link");

    // A share-link reader is not looking at a "team view", so the same
    // withholding names their door instead.
    expect(screen.getByText("Image not shared with view-only links.")).toBeInTheDocument();
    expect(screen.queryByText("Image not shared with team views.")).toBeNull();
    // The file marker is audience-neutral and identical either way.
    expect(
      screen.getByText(/daily_spend_by_channel\.csv \(file not shared\)/),
    ).toBeInTheDocument();
  });

  it("leaves the user's own words untouched", () => {
    renderTranscript();
    expect(
      screen.getByText("How did spend break down by channel?"),
    ).toBeInTheDocument();
  });
});

// The team door with the owner's shared outputs (B19): shared files are live
// downloads from the team-files route, unshared ones are locked names, and the
// public door ignores `sharedFiles` entirely — public links never expose files.
describe("ReadOnlyTranscript — shared outputs on the team door", () => {
  const CONV = "0f8fad5b-d9cb-469f-a165-70867728950e";
  const FILES_TRANSCRIPT = [
    {
      id: 1,
      role: "assistant",
      type: "text",
      content: {
        text: [
          "![Daily spend](daily_spend.png)",
          "",
          "Data: [daily_spend.csv](daily_spend.csv)",
          "",
          "Old list: [exclusion_list_v1.json](exclusion_list_v1.json)",
        ].join("\n"),
      },
    },
  ];
  const sharedFiles = {
    shared: new Set(["daily_spend.png", "daily_spend.csv"]),
    fileUrl: (p: string) => `/api/conversations/${CONV}/team-files/${encodeURIComponent(p)}`,
  };

  function renderWith(audience: ReadOnlyAudience) {
    return render(
      <ReadOnlyTranscript
        bubbles={toBubbles(FILES_TRANSCRIPT)}
        audience={audience}
        sharedFiles={sharedFiles}
        renderAssistant={(text) => renderAssistantContent(text, false, null)}
      />,
    );
  }

  it("makes a shared output a download from the team-files route", () => {
    renderWith("team");
    const link = screen.getByRole("link", { name: "daily_spend.csv" });
    expect(link).toHaveAttribute(
      "href",
      `/api/conversations/${CONV}/team-files/daily_spend.csv`,
    );
    expect(link).toHaveAttribute("download", "daily_spend.csv");
  });

  it("renders a shared image inline from the team-files route", () => {
    const { container } = renderWith("team");
    const img = container.querySelector("img");
    expect(img?.getAttribute("src")).toBe(
      `/api/conversations/${CONV}/team-files/daily_spend.png`,
    );
  });

  it("shows an unshared output as a locked name with no anchor", () => {
    const { container } = renderWith("team");
    const locked = screen.getByTestId("locked-file");
    expect(locked).toHaveTextContent("exclusion_list_v1.json (not shared)");
    expect(locked.querySelector("svg")).not.toBeNull();
    expect(container.querySelector('a[href*="exclusion_list"]')).toBeNull();
    expect(container.querySelector('a[href^="#"]')).toBeNull();
  });

  it("ignores shared files on a public link: transcript only", () => {
    const { container } = renderWith("link");
    expect(container.querySelector("a")).toBeNull();
    expect(container.querySelector("img")).toBeNull();
    expect(container.innerHTML).not.toContain("team-files");
    expect(screen.getByText(/daily_spend\.csv \(file not shared\)/)).toBeInTheDocument();
  });
});

// The decision is made on what the CommonMark parser RENDERS, not on regexes
// over the source (Codex round 7): label nesting, an escaped `]` and balanced
// parens in a destination are all links to remark — and to the server's
// goldmark discovery — so a shared output among them must be live, an
// unshared one locked, and the public link must never emit any of them.
describe("ReadOnlyTranscript — CommonMark-shaped references", () => {
  const CONV = "0f8fad5b-d9cb-469f-a165-70867728950e";
  const md = [
    "Nested: [outer [inner]](nested.csv)",
    "",
    "Escaped: [a\\]b](esc.csv)",
    "",
    "Parens: [p](foo(and(more)).csv)",
    "",
    "![chart [v2]](chart(1).png)",
    "",
    "[![thumb](thumb(1).png)](full(1).png)",
  ].join("\n");
  const entries = [{ id: 1, role: "assistant", type: "text", content: { text: md } }];
  const ALL = ["nested.csv", "esc.csv", "foo(and(more)).csv", "chart(1).png", "thumb(1).png", "full(1).png"];
  const fileUrl = (p: string) => `/api/conversations/${CONV}/team-files/${encodeURIComponent(p)}`;

  function renderWith(audience: ReadOnlyAudience, shared: string[]) {
    return render(
      <ReadOnlyTranscript
        bubbles={toBubbles(entries)}
        audience={audience}
        sharedFiles={{ shared: new Set(shared), fileUrl }}
        renderAssistant={(text) => renderAssistantContent(text, false, null)}
      />,
    );
  }

  it("makes each shared one a live team-files link or image", () => {
    const { container } = renderWith("team", ALL);
    expect(screen.getByRole("link", { name: "outer [inner]" })).toHaveAttribute(
      "href",
      fileUrl("nested.csv"),
    );
    expect(screen.getByRole("link", { name: "a]b" })).toHaveAttribute("href", fileUrl("esc.csv"));
    expect(screen.getByRole("link", { name: "p" })).toHaveAttribute(
      "href",
      fileUrl("foo(and(more)).csv"),
    );
    expect(screen.getByRole("link", { name: "p" })).toHaveAttribute("download", "foo(and(more)).csv");
    const srcs = Array.from(container.querySelectorAll("img")).map((i) => i.getAttribute("src"));
    expect(srcs).toEqual([fileUrl("chart(1).png"), fileUrl("thumb(1).png")]);
    // The thumbnail stays a thumbnail link to its full-size file.
    const thumb = container.querySelector(`img[src="${fileUrl("thumb(1).png")}"]`);
    expect(thumb?.closest("a")).toHaveAttribute("href", fileUrl("full(1).png"));
    expect(screen.queryByTestId("locked-file")).toBeNull();
  });

  it("locks each unshared one: a name, never an anchor or image", () => {
    const { container } = renderWith("team", []);
    const locked = screen.getAllByTestId("locked-file").map((l) => l.textContent);
    for (const name of ALL) expect(locked).toContain(`${name} (not shared)`);
    expect(container.querySelector("a")).toBeNull();
    expect(container.querySelector("img")).toBeNull();
  });

  it("never emits a workspace link or image on a public link, shared or not", () => {
    const { container } = renderWith("link", ALL);
    expect(container.querySelector("a")).toBeNull();
    expect(container.querySelector("img")).toBeNull();
    expect(container.innerHTML).not.toContain("team-files");
    expect(container.innerHTML).not.toContain("/workspace/");
  });

  it("enforces the public policy at render, independent of the source pre-pass", () => {
    // Markdown that reaches the renderer UNREDACTED (as if the regex pre-pass
    // missed every reference) still yields no workspace link or image.
    const { container } = render(
      <ReadOnlyFilesContext.Provider
        value={{ mode: "withhold", imagePlaceholder: "Image not shared with view-only links." }}
      >
        {renderAssistantContent(md, false, null)}
      </ReadOnlyFilesContext.Provider>,
    );
    expect(container.querySelector("a")).toBeNull();
    expect(container.querySelector("img")).toBeNull();
    expect(container.textContent).toContain("nested.csv (file not shared)");
    expect(container.textContent).toContain("foo(and(more)).csv (file not shared)");
    expect(container.textContent).toContain("Image not shared with view-only links.");
  });

  it("keeps code blocks, html previews and external links working", () => {
    const text = [
      "```python",
      "print('[x](data.csv)')",
      "```",
      "",
      "```html",
      "<p>hi</p>",
      "```",
      "",
      "[docs](https://example.com/a)",
    ].join("\n");
    const { container } = render(
      <ReadOnlyTranscript
        bubbles={toBubbles([{ id: 1, role: "assistant", type: "text", content: { text } }])}
        audience="team"
        sharedFiles={{ shared: new Set(), fileUrl }}
        renderAssistant={(t) => renderAssistantContent(t, false, null)}
      />,
    );
    expect(container.querySelector("pre")?.textContent).toContain("print('[x](data.csv)')");
    expect(container.querySelector("iframe[title='HTML preview']")).not.toBeNull();
    expect(screen.getByRole("link", { name: "docs" })).toHaveAttribute("href", "https://example.com/a");
  });
});

// A compaction summary starts a new message in the owner's chat. Readers who
// may not see it get a content-free boundary in its place, and the read-only
// renderer must split there too — otherwise an unclosed fence before the
// summary swallows a link after it, and the reader sees a different document
// than the owner (and than output discovery parses).
describe("toBubbles — summary boundaries", () => {
  const ENTRIES = [
    { id: 1, role: "user", type: "text", content: { text: "show me" } },
    { id: 2, role: "assistant", type: "text", content: { text: "raw:\n```\n" } },
    { id: 3, role: "assistant", type: "summary_boundary", content: {} },
    {
      id: 4,
      role: "assistant",
      type: "text",
      content: { text: "See [the docs](https://example.com/after)." },
    },
  ];

  it("ends the bubble at the boundary and never renders the boundary itself", () => {
    const bubbles = toBubbles(ENTRIES);
    expect(bubbles.map((b) => b.text)).toEqual([
      "show me",
      "raw:\n```\n",
      "See [the docs](https://example.com/after).",
    ]);
    expect(bubbles.map((b) => b.lastId)).toEqual([1, 2, 4]);
  });

  it("an unclosed fence before the boundary does not swallow the link after it", () => {
    render(
      <ReadOnlyTranscript
        bubbles={toBubbles(ENTRIES)}
        audience="team"
        renderAssistant={(text) => renderAssistantContent(text, false, null)}
      />,
    );
    expect(screen.getByRole("link", { name: "the docs" })).toHaveAttribute(
      "href",
      "https://example.com/after",
    );
  });

  it("a boundary with nothing after it adds no empty bubble", () => {
    const bubbles = toBubbles([
      { id: 1, role: "user", type: "text", content: { text: "hi" } },
      { id: 2, role: "assistant", type: "summary_boundary", content: {} },
    ]);
    expect(bubbles).toHaveLength(1);
  });
});

// `[![preview](private.png)](https://example.com)`: the outer link is
// external, the image inside is not live. The image's locked label must never
// sit inside a live anchor — the visible text would say "(not shared)" while a
// click went to an arbitrary URL. Every door splits it: the label as plain
// text, then the target as its own link whose visible text is the URL.
describe("a locked image inside an external link", () => {
  const CONV = "0f8fad5b-d9cb-469f-a165-70867728950e";
  const md = "[![preview](private.png)](https://example.com/x)";
  const entries = [{ id: 1, role: "assistant", type: "text", content: { text: md } }];
  const fileUrl = (p: string) => `/api/conversations/${CONV}/team-files/${encodeURIComponent(p)}`;

  function expectSplit(container: HTMLElement, lockedText: RegExp) {
    // The locked label is outside every anchor...
    const label = screen.getByText(lockedText);
    expect(label.closest("a")).toBeNull();
    // ...and the external target is its own link, named by its URL.
    const link = screen.getByRole("link", { name: "https://example.com/x" });
    expect(link).toHaveAttribute("href", "https://example.com/x");
    expect(link.textContent).toBe("https://example.com/x");
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelectorAll("a")).toHaveLength(1);
  }

  it("team view: locked label, then the external link on its own", () => {
    const { container } = render(
      <ReadOnlyTranscript
        bubbles={toBubbles(entries)}
        audience="team"
        sharedFiles={{ shared: new Set(), fileUrl }}
        renderAssistant={(t) => renderAssistantContent(t, false, null)}
      />,
    );
    expectSplit(container, /private\.png \(not shared\)/);
  });

  it("public link: the image placeholder is never inside the external anchor", () => {
    const { container } = render(
      <ReadOnlyFilesContext.Provider
        value={{ mode: "withhold", imagePlaceholder: "Image not shared with view-only links." }}
      >
        {renderAssistantContent(md, false, null)}
      </ReadOnlyFilesContext.Provider>,
    );
    expectSplit(container, /Image not shared with view-only links\./);
  });

  it("public link through the full view (source pre-pass included)", () => {
    const { container } = render(
      <ReadOnlyTranscript
        bubbles={toBubbles(entries)}
        audience="link"
        renderAssistant={(t) => renderAssistantContent(t, false, null)}
      />,
    );
    expectSplit(container, /Image not shared with view-only links\./);
  });

  it("teammate branch: a withheld thumbnail image splits the same way", () => {
    const { container } = render(
      <WithheldFilesContext.Provider
        value={{ conversationId: CONV, withheld: new Set(["private.png"]) }}
      >
        {renderAssistantContent(md, false, CONV)}
      </WithheldFilesContext.Provider>,
    );
    expectSplit(container, /private\.png \(not shared\)/);
  });

  it("team view: an image nested under emphasis splits too", () => {
    const nested = [
      { id: 1, role: "assistant", type: "text", content: { text: "[**![preview](private.png)**](https://example.com/x)" } },
    ];
    const { container } = render(
      <ReadOnlyTranscript
        bubbles={toBubbles(nested)}
        audience="team"
        sharedFiles={{ shared: new Set(), fileUrl }}
        renderAssistant={(t) => renderAssistantContent(t, false, null)}
      />,
    );
    expectSplit(container, /private\.png \(not shared\)/);
  });

  it("team view: a locked image after a shared one still splits the link", () => {
    const mixed = [
      {
        id: 1,
        role: "assistant",
        type: "text",
        content: { text: "[![a](shared.png) ![b](private.png)](https://example.com/x)" },
      },
    ];
    const { container } = render(
      <ReadOnlyTranscript
        bubbles={toBubbles(mixed)}
        audience="team"
        sharedFiles={{ shared: new Set(["shared.png"]), fileUrl }}
        renderAssistant={(t) => renderAssistantContent(t, false, null)}
      />,
    );
    expect(screen.getByText(/private\.png \(not shared\)/).closest("a")).toBeNull();
    const link = screen.getByRole("link", { name: "https://example.com/x" });
    expect(link.textContent).toBe("https://example.com/x");
    expect(container.querySelectorAll("a")).toHaveLength(1);
  });

  it("teammate branch: a nested withheld image splits too", () => {
    const { container } = render(
      <WithheldFilesContext.Provider
        value={{ conversationId: CONV, withheld: new Set(["private.png"]) }}
      >
        {renderAssistantContent("[**![preview](private.png)**](https://example.com/x)", false, CONV)}
      </WithheldFilesContext.Provider>,
    );
    expectSplit(container, /private\.png \(not shared\)/);
  });

  it("a live image inside an external link stays one thumbnail link", () => {
    const { container } = render(
      <ReadOnlyTranscript
        bubbles={toBubbles([
          {
            id: 1,
            role: "assistant",
            type: "text",
            content: { text: "[![p](https://cdn.example.com/p.png)](https://example.com/x)" },
          },
        ])}
        audience="team"
        sharedFiles={{ shared: new Set(), fileUrl }}
        renderAssistant={(t) => renderAssistantContent(t, false, null)}
      />,
    );
    expect(container.querySelector("img")?.closest("a")).toHaveAttribute(
      "href",
      "https://example.com/x",
    );
  });
});

describe("toBubbles and card messages", () => {
  it("never merges a card answer into the request before it", () => {
    const bubbles = toBubbles([
      { role: "user", type: "text", content: { text: "set up deals" } },
      { role: "user", type: "text", content: { text: '[UI submission] card=c1 action=go\n```json\n{"a":1}\n```' } },
      { role: "user", type: "text", content: { text: "[UI reply] card=c1 action=no\nNot now" } },
    ]);
    expect(bubbles.map((b) => b.text.split("\n")[0])).toEqual([
      "set up deals",
      "[UI submission] card=c1 action=go",
      "[UI reply] card=c1 action=no",
    ]);
  });
});
