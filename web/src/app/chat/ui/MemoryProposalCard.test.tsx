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
// The default is MY MEMORY even in a team-shared project. This card holds text
// the MODEL extracted from the turn, not text the user picked, and publishing
// is one-way in the direction that matters — a retired team learning was still
// read. People have been clicking Save on these cards to mean "keep this"; a
// default that published to the whole project would turn that habit into a
// disclosure the first time the model lifted something sensitive out of a
// conversation. The team is one visible click away. (The explicit Save action
// on a message the user chose defaults the other way — see ChatTranscript.)
describe("MemoryProposalCard destination", () => {
  const project = { id: "p1", name: "Quant", teamShared: true };

  it("has no destination picker outside a project", () => {
    render(<MemoryProposalCard proposal={base} onResolved={() => {}} />);
    expect(screen.queryByRole("group", { name: /where to save/i })).toBeNull();
  });

  it("offers team learnings but does not preselect it, and posts the project id when chosen", async () => {
    const fetchMock = mockFetch({ supersede: "none" });
    const resolved: MemoryProposal[] = [];
    render(
      <MemoryProposalCard
        proposal={base}
        project={project}
        onResolved={(next) => resolved.push(next)}
      />,
    );

    expect(screen.getByRole("button", { name: "My memory" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    fireEvent.click(screen.getByRole("button", { name: "Team learnings" }));
    expect(
      screen.getByRole("button", { name: "Team learnings" }),
    ).toHaveAttribute("aria-pressed", "true");
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(JSON.parse(String(init.body))).toEqual({ project_id: "p1" });
    await waitFor(() => expect(resolved).toHaveLength(1));
    expect(resolved[0].savedTo).toBe("Quant");
  });

  it("saving without touching the picker posts no project id", async () => {
    const fetchMock = mockFetch({ supersede: "none" });
    render(
      <MemoryProposalCard proposal={base} project={project} onResolved={() => {}} />,
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

  it("defaults to My memory in a personal project", () => {
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
  });
});
