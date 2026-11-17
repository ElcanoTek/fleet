import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describeSubmitFailure, MemoryProposalCard } from "./ApprovalCards";
import type { MemoryProposal } from "./history";

// A card left open past the session's lifetime (ADR-0064) answered every
// click with "HTTP 401 … Try again", which no retry could fix. A 401 now
// starts the sign-in navigation; any other failure keeps its message.

const assign = vi.fn();

beforeEach(() => {
  vi.stubGlobal("location", { ...window.location, assign });
});

afterEach(() => {
  cleanup();
  assign.mockReset();
  vi.unstubAllGlobals();
});

function reply(status: number, body = "") {
  const fn = vi.fn(async () => new Response(body, { status }));
  vi.stubGlobal("fetch", fn);
  return fn;
}

const proposal: MemoryProposal = {
  id: "prop-1",
  content: "daily patrol reports at 8:00 AM",
  kind: "preference",
  status: "pending",
};

describe("describeSubmitFailure", () => {
  it("sends an expired session to sign-in", async () => {
    const text = await describeSubmitFailure(
      new Response(JSON.stringify({ error: "Unauthorized" }), { status: 401 }),
    );
    expect(assign).toHaveBeenCalledWith("/login");
    expect(text).toMatch(/sign-in expired/);
    expect(text).not.toMatch(/HTTP 401/);
  });

  it("keeps the status line for any other failure", async () => {
    const text = await describeSubmitFailure(new Response("approval expired", { status: 409 }));
    expect(assign).not.toHaveBeenCalled();
    expect(text).toBe("Couldn't submit your decision (HTTP 409): approval expired");
  });
});

describe("MemoryProposalCard on an expired session", () => {
  it("sends Save to sign-in and leaves the proposal pending", async () => {
    reply(401, JSON.stringify({ error: "Unauthorized" }));
    const onResolved = vi.fn();
    render(<MemoryProposalCard proposal={proposal} onResolved={onResolved} />);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/login"));
    expect(screen.getByText(/sign-in expired/)).toBeInTheDocument();
    expect(onResolved).not.toHaveBeenCalled();
  });

  it("sends Don't save to sign-in too", async () => {
    reply(401);
    const onResolved = vi.fn();
    render(<MemoryProposalCard proposal={proposal} onResolved={onResolved} />);
    fireEvent.click(screen.getByRole("button", { name: "Don't save" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/login"));
    expect(onResolved).not.toHaveBeenCalled();
  });

  it("still reports a non-401 save failure without navigating", async () => {
    reply(500);
    render(<MemoryProposalCard proposal={proposal} onResolved={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(screen.getByText("Couldn't save this memory (HTTP 500).")).toBeInTheDocument());
    expect(assign).not.toHaveBeenCalled();
  });
});
