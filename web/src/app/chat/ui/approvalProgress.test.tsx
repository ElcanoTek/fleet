import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import { ApprovalCard } from "./ApprovalCards";
import { approvalProgressText, hydrateResolvedApproval, parseApprovalProgress, type Approval } from "./history";
import { APPROVAL_PROGRESS_POLL_MS, approvalFromPoll } from "./useApprovalProgressPoll";

// Approval progress (docs/APPROVAL-PROGRESS.md): a running approved call of a
// tool in critical_tool_progress shows its MCP progress, and its card polls
// the one-card GET for the progress and the outcome.

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function running(extra: Partial<Approval> = {}): Approval {
  return {
    id: "ap-run",
    tool: "mcp_pubmatic_execute_plan",
    summary: { tool: "mcp_pubmatic_execute_plan", args: [{ key: "plan_id", value: "p1" }] },
    status: "pending",
    executing: true,
    progressUpdates: true,
    ...extra,
  };
}

describe("progress payloads", () => {
  it("accept finite non-negative numbers and render the count and message", () => {
    const p = parseApprovalProgress({ progress: 12, total: 24, message: "creating deal 12", updated_at: 5 });
    expect(p).toEqual({ progress: 12, total: 24, message: "creating deal 12", updatedAt: 5 });
    expect(approvalProgressText(p!)).toBe("12 of 24 · creating deal 12");
    expect(approvalProgressText({ progress: 3 })).toBe("3");
    expect(approvalProgressText({ progress: 0.5, total: 2 })).toBe("0.5 of 2");
  });

  it("drop anything off-shape", () => {
    for (const bad of [null, "x", { progress: -1 }, { progress: Infinity }, { progress: 1, total: "2" }, { progress: 1, message: 3 }]) {
      expect(parseApprovalProgress(bad)).toBeUndefined();
    }
  });

  it("hydrate on an executing resolved row only", () => {
    const base = { approval_id: "a", tool: "t", summary: {}, status: "approved" as const, progress_updates: true, progress: { progress: 1, total: 2 } };
    expect(hydrateResolvedApproval({ ...base, executing: true }).progress).toEqual({ progress: 1, total: 2 });
    expect(hydrateResolvedApproval({ ...base, executing: true }).progressUpdates).toBe(true);
    expect(hydrateResolvedApproval({ ...base, is_err: false }).progress).toBeUndefined();
  });
});

describe("approvalFromPoll", () => {
  it("takes new progress, ignores the same progress, and settles on the outcome", () => {
    const cur = running({ progress: { progress: 1, total: 24 } });
    const moved = approvalFromPoll(cur, { resolved_approvals: [{ approval_id: "ap-run", status: "approved", executing: true, progress: { progress: 12, total: 24 } }] });
    expect(moved?.progress).toEqual({ progress: 12, total: 24 });
    expect(approvalFromPoll(cur, { resolved_approvals: [{ approval_id: "ap-run", status: "approved", executing: true, progress: { progress: 1, total: 24 } }] })).toBeNull();
    const done = approvalFromPoll(cur, { resolved_approvals: [{ approval_id: "ap-run", status: "approved", is_err: false, result_text: "24 created" }] });
    expect(done).toMatchObject({ status: "approved", executing: false, resultText: "24 created", progress: undefined });
    const failed = approvalFromPoll(cur, { resolved_approvals: [{ approval_id: "ap-run", status: "approved", is_err: true, result_text: "boom" }] });
    expect(failed?.status).toBe("failed");
    expect(approvalFromPoll(cur, { resolved_approvals: [] })).toBeNull();
  });
});

describe("a running card of a tool that reports progress", () => {
  it("shows the progress bar and text", () => {
    render(<ApprovalCard approval={running({ progress: { progress: 12, total: 24, message: "creating deal 12" } })} conversationId="conv_1" onResolved={vi.fn()} />);
    expect(screen.getByTestId("approval-progress-text").textContent).toBe("12 of 24 · creating deal 12");
    expect(screen.getByRole("progressbar").getAttribute("aria-valuenow")).toBe("50");
    expect(screen.getByTestId("approval-check-result")).toBeTruthy();
  });

  it("polls the one-card GET and reports progress, then the outcome", async () => {
    vi.useFakeTimers();
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ pending_approvals: [], resolved_approvals: [{ approval_id: "ap-run", status: "approved", executing: true, progress: { progress: 12, total: 24 } }] }), { status: 200 }),
      )
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ pending_approvals: [], resolved_approvals: [{ approval_id: "ap-run", status: "approved", is_err: false, result_text: "24 created" }] }), { status: 200 }),
      );
    vi.stubGlobal("fetch", fetchMock);
    const onResolved = vi.fn();
    render(<ApprovalCard approval={running()} conversationId="conv_1" onResolved={onResolved} />);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(APPROVAL_PROGRESS_POLL_MS);
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(String(fetchMock.mock.calls[0][0])).toBe("/api/conversations/conv_1/approvals/ap-run");
    expect(onResolved).toHaveBeenLastCalledWith(expect.objectContaining({ executing: true, progress: { progress: 12, total: 24 } }));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(APPROVAL_PROGRESS_POLL_MS);
    });
    expect(onResolved).toHaveBeenLastCalledWith(expect.objectContaining({ status: "approved", resultText: "24 created" }));
  });

  it("does not poll for a tool that does not report progress, or a card that is not running", async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    render(<ApprovalCard approval={running({ progressUpdates: undefined })} conversationId="conv_1" onResolved={vi.fn()} />);
    render(<ApprovalCard approval={running({ id: "ap-2", executing: undefined })} conversationId="conv_1" onResolved={vi.fn()} />);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3 * APPROVAL_PROGRESS_POLL_MS);
    });
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
