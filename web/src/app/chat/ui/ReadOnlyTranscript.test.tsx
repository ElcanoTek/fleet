import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { renderAssistantContent } from "./AssistantContent";
import { ReadOnlyTranscript, toBubbles, type ReadOnlyAudience } from "./ReadOnlyTranscript";

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
