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
