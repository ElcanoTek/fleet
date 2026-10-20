import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ApprovalCard } from "./ApprovalCards";
import { remainingAfterStop, retryFailedPrompt } from "./ApprovalResultCard";
import { hydrateResolvedApproval, type Approval } from "./history";
import { approvalFromPoll } from "./useApprovalProgressPoll";

// Stopping a running approved call (docs/APPROVAL-STOP.md): a running card of
// a tool in critical_tool_stop offers Stop; pressing it posts to the stop
// route and the card reads "Stopping" until the call settles; a settled card
// of a stopped call offers "Run the remaining N" for the items it never
// reached.

function running(overrides: Partial<Approval> = {}): Approval {
  return {
    id: "ap_1",
    tool: "mcp_ix_execute_plan",
    status: "pending",
    executing: true,
    stoppable: true,
    summary: { tool: "mcp_ix_execute_plan", args: [{ key: "deal_ids", value: '["k1","k2","k3"]' }] },
    card: { title: "Update floor on 3 Index Exchange deals", items: [{ key: "k1", label: "Deal one" }] },
    ...overrides,
  };
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("the running card's Stop", () => {
  it("posts the stop and shows Stopping while the call finishes its current item", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ status: "approved", executing: true, stopping: true, stoppable: true, result_text: "Approved — executing…" }), {
        status: 200,
      }),
    );
    vi.stubGlobal("fetch", fetchMock);
    const onResolved = vi.fn();
    render(<ApprovalCard approval={running()} conversationId="conv_1" onResolved={onResolved} />);
    const stop = screen.getByTestId("approval-stop");
    expect(stop).toHaveAccessibleDescription("Stopping finishes the current item, then halts.");
    fireEvent.click(stop);
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(fetchMock.mock.calls[0][0]).toBe("/api/conversations/conv_1/approvals/ap_1/stop");
    expect(fetchMock.mock.calls[0][1]).toMatchObject({ method: "POST" });
    await waitFor(() => expect(onResolved).toHaveBeenCalled());
    expect(onResolved.mock.calls[0][0]).toMatchObject({ status: "pending", executing: true, stopping: true });
  });

  it("reads Stopping and offers no second Stop once stopping", () => {
    render(<ApprovalCard approval={running({ stopping: true })} conversationId="conv_1" onResolved={vi.fn()} />);
    expect(screen.getByTestId("approval-stopping")).toHaveTextContent("Stopping: the current item finishes, then it halts.");
    expect(screen.queryByTestId("approval-stop")).toBeNull();
  });

  it("offers no Stop for a tool that did not opt in", () => {
    render(<ApprovalCard approval={running({ stoppable: undefined })} conversationId="conv_1" onResolved={vi.fn()} />);
    expect(screen.getByTestId("approval-check-result")).toBeTruthy();
    expect(screen.queryByTestId("approval-stop")).toBeNull();
  });

  it("keeps the card pending with the server's reason when the stop is refused", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("this action is not running", { status: 409 })));
    const onResolved = vi.fn();
    render(<ApprovalCard approval={running()} conversationId="conv_1" onResolved={onResolved} />);
    fireEvent.click(screen.getByTestId("approval-stop"));
    await waitFor(() => expect(screen.getByText(/not running/)).toBeTruthy());
    expect(onResolved).not.toHaveBeenCalled();
  });
});

describe("a stopped call", () => {
  const resultCard = {
    outcome: "partial" as const,
    title: "Stopped after 1 of 3 · 1 updated",
    items: [
      { key: "k1", label: "Deal one", status: "done" as const },
      { key: "k2", label: "Deal two", status: "not_run" as const },
      { key: "k3", label: "Deal three", status: "not_run" as const },
    ],
    retryKeys: ["k2", "k3"],
  };

  it("hydrates stopping and stopped from the server's rows", () => {
    const run = hydrateResolvedApproval({ approval_id: "a", tool: "t", summary: {}, status: "approved", executing: true, stoppable: true, stopping: true });
    expect(run).toMatchObject({ stoppable: true, stopping: true });
    const done = hydrateResolvedApproval({ approval_id: "a", tool: "t", summary: {}, status: "approved", is_err: false, stoppable: true, stopped: true });
    expect(done).toMatchObject({ stopped: true });
    expect(done.stopping).toBeUndefined();
  });

  it("offers Run the remaining N, worded for the items the stop left", () => {
    const approval = running({ status: "approved", executing: false, stopped: true, resultCard, resultText: "{}" });
    expect(remainingAfterStop(approval, ["k2", "k3"])).toBe(true);
    expect(remainingAfterStop({ ...approval, stopped: undefined }, ["k2", "k3"])).toBe(false);
    expect(retryFailedPrompt(approval, ["k2", "k3"])).toContain("Run the remaining 2 items of the approved mcp_ix_execute_plan call (approval_id=ap_1) that were not run because I stopped it");
    render(<ApprovalCard approval={approval} conversationId="conv_1" onResolved={vi.fn()} onRetryFailed={vi.fn()} />);
    expect(screen.getByTestId("approval-retry-failed")).toHaveTextContent("Run the remaining 2");
  });

  it("the poll carries a stop pressed elsewhere, then the stopped outcome", () => {
    const current = running();
    const next = approvalFromPoll(current, { resolved_approvals: [{ approval_id: "ap_1", status: "approved", executing: true, stopping: true }] });
    expect(next).toMatchObject({ executing: true, stopping: true });
    const settled = approvalFromPoll(next as Approval, { resolved_approvals: [{ approval_id: "ap_1", status: "approved", is_err: false, stopped: true }] });
    expect(settled).toMatchObject({ status: "approved", executing: false, stopped: true });
  });
});
