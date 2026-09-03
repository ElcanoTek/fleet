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
// the answer before the user saves rather than defaulting silently. Inside a
// TEAM-shared project the team is preselected — that is what a member working
// there usually means — and the choice is still visible and flippable.
describe("MemoryProposalCard destination", () => {
  const project = { id: "p1", name: "Quant", teamShared: true };

  it("has no destination picker outside a project", () => {
    render(<MemoryProposalCard proposal={base} onResolved={() => {}} />);
    expect(screen.queryByRole("group", { name: /where to save/i })).toBeNull();
  });

  it("preselects team learnings in a team-shared project and posts the project id", async () => {
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
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(JSON.parse(String(init.body))).toEqual({ project_id: "p1" });
    await waitFor(() => expect(resolved).toHaveLength(1));
    expect(resolved[0].savedTo).toBe("Quant");
  });

  it("flipping to My memory posts no project id", async () => {
    const fetchMock = mockFetch({ supersede: "none" });
    render(
      <MemoryProposalCard proposal={base} project={project} onResolved={() => {}} />,
    );

    fireEvent.click(screen.getByRole("button", { name: "My memory" }));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(fetchMock).toHaveBeenCalled());
    const [, init] = fetchMock.mock.calls[0] as unknown as [string, RequestInit];
    expect(JSON.parse(String(init.body))).toEqual({});
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
