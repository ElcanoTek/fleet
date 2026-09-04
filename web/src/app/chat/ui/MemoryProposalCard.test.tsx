import { describe, expect, it, vi, afterEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryProposalCard } from "./ApprovalCards";
import type { MemoryProposal } from "./history";

// #515 stage 2: the Save/Don't-Save card renders a supersede claim and
// surfaces the accept endpoint's outcome for the older fact.

function mockFetch(body: unknown, ok = true) {
  const fn = vi.fn(async () =>
    new Response(JSON.stringify(body), { status: ok ? 200 : 400 }),
  );
  vi.stubGlobal("fetch", fn);
  return fn;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

const base: MemoryProposal = {
  id: "prop-1",
  content: "office is in Austin",
  kind: "fact",
  status: "pending",
};

describe("MemoryProposalCard", () => {
  it("renders the replaces line for a supersede claim", () => {
    render(
      <MemoryProposalCard
        proposal={{ ...base, supersedesContent: "office is in Boston" }}
        onResolved={() => {}}
      />,
    );
    expect(screen.getByText(/Replaces:/)).toBeInTheDocument();
    expect(screen.getByText("office is in Boston")).toBeInTheDocument();
    expect(screen.getByText(/retired only if you save/)).toBeInTheDocument();
  });

  it("omits the replaces line without a claim", () => {
    render(<MemoryProposalCard proposal={base} onResolved={() => {}} />);
    expect(screen.queryByText(/Replaces:/)).not.toBeInTheDocument();
  });

  it("surfaces the retired outcome after save", async () => {
    mockFetch({ memory: {}, supersede: "retired" });
    const onResolved = vi.fn();
    render(
      <MemoryProposalCard
        proposal={{ ...base, supersedesContent: "office is in Boston" }}
        onResolved={onResolved}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(onResolved).toHaveBeenCalled());
    expect(onResolved).toHaveBeenCalledWith(
      expect.objectContaining({
        status: "saved",
        resolutionNote: expect.stringContaining("Replaced the older fact"),
      }),
    );
  });

  it("surfaces the pinned guard after save", async () => {
    mockFetch({ memory: {}, supersede: "target_pinned" });
    const onResolved = vi.fn();
    render(
      <MemoryProposalCard
        proposal={{ ...base, supersedesContent: "timezone is EST" }}
        onResolved={onResolved}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(onResolved).toHaveBeenCalled());
    expect(onResolved).toHaveBeenCalledWith(
      expect.objectContaining({
        status: "saved",
        resolutionNote: expect.stringContaining("pinned"),
      }),
    );
  });

  it("save without a claim carries no note", async () => {
    mockFetch({ memory: {}, supersede: "" });
    const onResolved = vi.fn();
    render(<MemoryProposalCard proposal={base} onResolved={onResolved} />);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(onResolved).toHaveBeenCalled());
    expect(onResolved).toHaveBeenCalledWith(
      expect.objectContaining({ status: "saved", resolutionNote: undefined }),
    );
  });
});

// Item D1: inside a project the card asks WHERE the memory goes, and shows
// the answer before the user saves rather than defaulting silently.
//
// Inside a TEAM-SHARED project the preselected destination is Team learnings
// (docs/TEAM-SHARING.md, "Capturing one"). Finding #17: it used to preselect
// My memory, which disagreed with the context and with the assistant's own
// framing — "remember that this project is for testing purposes" is a fact
// about the project, and one lazy click filed it privately. Outside a
// team-shared project there is no team to learn anything, so My memory is the
// preselection. Either way the picker is visible and one click flips it.
describe("MemoryProposalCard destination", () => {
  const project = { id: "p1", name: "Quant", teamShared: true };

  it("has no destination picker outside a project", () => {
    render(<MemoryProposalCard proposal={base} onResolved={() => {}} />);
    expect(screen.queryByRole("group", { name: /where to save/i })).toBeNull();
  });

  it("preselects team learnings in a team-shared project, and posts the project id", async () => {
    const fetchMock = mockFetch({ supersede: "none" });
    const resolved: MemoryProposal[] = [];
    render(
      <MemoryProposalCard
        proposal={base}
        project={project}
        onResolved={(next) => resolved.push(next)}
      />,
    );

    expect(
      screen.getByRole("button", { name: "Team learnings" }),
    ).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: "My memory" })).toHaveAttribute(
      "aria-pressed",
      "false",
    );
    // The helper text switches with the selection, so the destination is
    // stated in words as well as in the toggle.
    expect(screen.getByText("everyone in Quant")).toBeInTheDocument();

    // Saving with the preselection is what the finding is about: no extra
    // click, and the project id goes with it.
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(JSON.parse(String(init.body))).toEqual({ project_id: "p1" });
    await waitFor(() => expect(resolved).toHaveLength(1));
    expect(resolved[0].savedTo).toBe("Quant");
  });

  it("flips to my memory on one click, and then posts no project id", async () => {
    const fetchMock = mockFetch({ supersede: "none" });
    render(
      <MemoryProposalCard proposal={base} project={project} onResolved={() => {}} />,
    );

    fireEvent.click(screen.getByRole("button", { name: "My memory" }));
    expect(screen.getByRole("button", { name: "My memory" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(screen.getByText("only you, in every chat")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(JSON.parse(String(init.body))).toEqual({});
  });

  it("adopts a project that arrives after the first render", async () => {
    // On a boot restore the projects list can land after the card has already
    // painted, so the preselection is synced rather than read once at mount.
    const { rerender } = render(
      <MemoryProposalCard proposal={base} onResolved={() => {}} />,
    );
    expect(screen.queryByRole("group", { name: /where to save/i })).toBeNull();
    rerender(
      <MemoryProposalCard proposal={base} project={project} onResolved={() => {}} />,
    );
    expect(
      screen.getByRole("button", { name: "Team learnings" }),
    ).toHaveAttribute("aria-pressed", "true");
  });

  it("does not stomp an explicit choice when the project arrives late", async () => {
    const fetchMock = mockFetch({ supersede: "none" });
    const { rerender } = render(
      <MemoryProposalCard
        proposal={base}
        project={{ ...project, teamShared: false }}
        onResolved={() => {}}
      />,
    );
    // The user picks Team learnings while the project still reads personal…
    fireEvent.click(screen.getByRole("button", { name: "Team learnings" }));
    // …and then the real (team-shared) project lands.
    rerender(
      <MemoryProposalCard proposal={base} project={project} onResolved={() => {}} />,
    );
    expect(
      screen.getByRole("button", { name: "Team learnings" }),
    ).toHaveAttribute("aria-pressed", "true");

    // …and the same guard in the other direction: an explicit My memory
    // survives the project turning out to be team-shared.
    fireEvent.click(screen.getByRole("button", { name: "My memory" }));
    rerender(
      <MemoryProposalCard
        proposal={base}
        project={{ ...project, name: "Quant II" }}
        onResolved={() => {}}
      />,
    );
    expect(screen.getByRole("button", { name: "My memory" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(JSON.parse(String(init.body))).toEqual({});
  });

  // A failed save is not a dismissal. Reporting "Dismissed." told the user
  // they had thrown the memory away when nothing had been written — and the
  // team destination has its own failure (a membership re-check server side)
  // that this card is the only place to see.
  it("reports a failed save as a failure, and leaves the card actionable", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response("not found", { status: 404 })),
    );
    const resolved: MemoryProposal[] = [];
    render(
      <MemoryProposalCard
        proposal={base}
        project={project}
        onResolved={(next) => resolved.push(next)}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Team learnings" }));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    expect(await screen.findByRole("alert")).toHaveTextContent(
      /no longer be a member/i,
    );
    expect(resolved).toHaveLength(0);
    expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();
    expect(screen.queryByText("Dismissed.")).toBeNull();
  });

  it("preselects My memory in a personal project", () => {
    // A personal project has no team to learn anything.
    render(
      <MemoryProposalCard
        proposal={base}
        project={{ ...project, teamShared: false }}
        onResolved={() => {}}
      />,
    );
    expect(screen.getByRole("button", { name: "My memory" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(
      screen.getByRole("button", { name: "Team learnings" }),
    ).toHaveAttribute("aria-pressed", "false");
  });
});
