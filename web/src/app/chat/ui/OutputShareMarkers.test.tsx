import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { renderAssistantContent } from "./AssistantContent";
import {
  OutputShareContext,
  workspacePathFromHref,
  type OutputShareMarkers,
} from "./OutputShareMarkers";

// B17 (#40): in an owner's team-shared chat each output chip carries a small
// "Shared" / "Not shared" marker that opens Sources. Private chats (no
// context) and non-outputs render exactly as before.

const CONV = "11111111-2222-3333-4444-555555555555";

function renderWith(md: string, markers: OutputShareMarkers | null) {
  return render(
    <OutputShareContext.Provider value={markers}>
      {renderAssistantContent(md, false, CONV)}
    </OutputShareContext.Provider>,
  );
}

const markers = (over: Partial<OutputShareMarkers> = {}): OutputShareMarkers => ({
  conversationId: CONV,
  shared: new Map([
    ["out/report.xlsx", true],
    ["draft notes.md", false],
  ]),
  team: "Elcano",
  onOpenSources: vi.fn(),
  ...over,
});

afterEach(cleanup);

describe("output share markers", () => {
  it("marks a shared output and opens Sources at that file", () => {
    const m = markers();
    renderWith("See [the report](out/report.xlsx).", m);
    const marker = screen.getByRole("button", {
      name: "report.xlsx is shared with Elcano. Manage in Sources",
    });
    expect(marker).toHaveTextContent("Shared");
    fireEvent.click(marker);
    expect(m.onOpenSources).toHaveBeenCalledWith("out/report.xlsx");
  });

  it("marks an unshared output, matching a percent-encoded link", () => {
    renderWith("[notes](draft%20notes.md)", markers());
    expect(
      screen.getByRole("button", { name: "draft notes.md is not shared. Manage in Sources" }),
    ).toHaveTextContent("Not shared");
  });

  it("leaves non-outputs and external links unmarked", () => {
    renderWith(
      "[upload](attachments/brief.pdf) and [site](https://example.com)",
      markers(),
    );
    expect(screen.queryByTestId("output-share-marker")).toBeNull();
  });

  it("changes nothing in a private chat (no context)", () => {
    renderWith("[the report](out/report.xlsx)", null);
    expect(screen.queryByTestId("output-share-marker")).toBeNull();
    expect(screen.getByRole("link", { name: "the report" })).toBeInTheDocument();
  });

  it("workspacePathFromHref inverts the workspace rewrite", () => {
    expect(
      workspacePathFromHref(`/api/conversations/${CONV}/workspace/a%20b/c.csv`, CONV),
    ).toBe("a b/c.csv");
    expect(workspacePathFromHref("https://example.com/x", CONV)).toBeNull();
  });
});
