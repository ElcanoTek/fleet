import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { SaveToMemoryAction } from "./ChatTranscript";

// Item D1's SECOND capture path: a Save action beside Copy · Regenerate ·
// Branch, for keeping something the agent said without composing a "remember
// this" turn. It shipped with no test at all — MemoryProposalCard covers only
// the approval-card path, which is a different component with a different
// default.

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function mockPost() {
  const calls: { url: string; body: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ url: String(input), body: String(init?.body ?? "") });
      return new Response(JSON.stringify({}), { status: 200 });
    }),
  );
  return calls;
}

describe("SaveToMemoryAction", () => {
  it("saves straight to personal memory outside a project — no choice worth showing", async () => {
    const calls = mockPost();
    const onSaved = vi.fn();
    render(<SaveToMemoryAction content="the spread is 12bps" project={null} onSaved={onSaved} />);

    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0].url).toBe("/api/memories");
    expect(JSON.parse(calls[0].body)).toEqual({
      content: "the spread is 12bps",
      kind: "fact",
    });
    expect(onSaved).toHaveBeenCalled();
    expect(await screen.findByText("Saved ✓")).toBeInTheDocument();
  });

  it("preselects the team in a team-shared project — the user picked this text", async () => {
    const calls = mockPost();
    render(
      <SaveToMemoryAction
        content="quote spreads in bps"
        project={{ id: "p1", name: "Quant", teamShared: true }}
        onSaved={vi.fn()}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    // The destination is shown BEFORE saving, never applied silently.
    expect(screen.getByRole("button", { name: "Team learnings" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    fireEvent.click(screen.getAllByRole("button", { name: "Save" })[1]);

    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0].url).toBe("/api/projects/p1/memories");
  });

  it("defaults to my memory in a personal project, and honours a flip", async () => {
    const calls = mockPost();
    render(
      <SaveToMemoryAction
        content="a note"
        project={{ id: "p2", name: "Mine", teamShared: false }}
        onSaved={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(screen.getByRole("button", { name: "My memory" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    fireEvent.click(screen.getByRole("button", { name: "Team learnings" }));
    fireEvent.click(screen.getAllByRole("button", { name: "Save" })[1]);

    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0].url).toBe("/api/projects/p2/memories");
  });

  it("says the save failed instead of claiming it worked", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("nope", { status: 403 })),
    );
    const onSaved = vi.fn();
    render(<SaveToMemoryAction content="x" project={null} onSaved={onSaved} />);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByRole("button", { name: "Save failed" })).toBeInTheDocument();
    expect(onSaved).not.toHaveBeenCalled();
  });
});
