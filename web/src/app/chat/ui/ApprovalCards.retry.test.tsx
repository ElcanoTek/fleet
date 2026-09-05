import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ApprovalCard } from "./ApprovalCards";
import type { Approval } from "./history";

// A failed approval POST (non-2xx or a dropped connection) used to resolve the
// card as "failed" locally, removing Approve/Deny while the server still held
// the approval pending — the agent kept waiting for an answer the user could
// no longer give, until the countdown auto-denied it. The card must stay
// pending, say what went wrong, and let the user retry; only a status the
// server actually reports moves it out of pending.

function renderCard(approval: Partial<Approval> & Pick<Approval, "tool">, onResolved = vi.fn()) {
  const full: Approval = {
    id: "ap_1",
    status: "pending",
    summary: {},
    ...approval,
  } as Approval;
  render(<ApprovalCard approval={full} conversationId="conv_1" onResolved={onResolved} />);
  return onResolved;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("approval cards keep a pending card pending when the POST fails", () => {
  it("shows the failure inline and re-enables the buttons after a non-2xx", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response("approval store unavailable", { status: 503 }))
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ status: "approved", result_text: "ran" }), { status: 200 }),
      );
    vi.stubGlobal("fetch", fetchMock);
    const onResolved = renderCard({
      tool: "mcp_pages_deploy_page",
      summary: { tool: "mcp_pages_deploy_page", args: [{ key: "slug", value: "q3" }] },
    });

    const approve = screen.getByRole("button", { name: "Approve & run" });
    fireEvent.click(approve);
    const error = await screen.findByTestId("approval-submit-error");
    expect(error).toHaveTextContent("HTTP 503");
    expect(error).toHaveTextContent("approval store unavailable");
    // Still a decision to make: nothing was stamped locally.
    expect(onResolved).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Approve & run" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeEnabled();

    // The retry is the same click, and the server's answer is what resolves it.
    fireEvent.click(screen.getByRole("button", { name: "Approve & run" }));
    await waitFor(() => expect(onResolved).toHaveBeenCalledTimes(1));
    expect(onResolved.mock.calls[0][0]).toMatchObject({ status: "approved", resultText: "ran" });
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("treats a network failure the same way", async () => {
    vi.stubGlobal("fetch", vi.fn().mockRejectedValue(new TypeError("Failed to fetch")));
    const onResolved = renderCard({ tool: "bash", summary: { command: "ls" } });

    fireEvent.click(screen.getByRole("button", { name: "Approve & run" }));
    const error = await screen.findByTestId("approval-submit-error");
    expect(error).toHaveTextContent("Couldn't reach the server");
    expect(onResolved).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Approve & run" })).toBeEnabled();
  });

  it("clears the previous failure when a retry is in flight", async () => {
    let release: (r: Response) => void = () => {};
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValueOnce(new Response("nope", { status: 500 }))
        .mockImplementationOnce(() => new Promise<Response>((resolve) => (release = resolve))),
    );
    renderCard({ tool: "bash", summary: { command: "ls" } });
    fireEvent.click(screen.getByRole("button", { name: "Approve & run" }));
    await screen.findByTestId("approval-submit-error");
    fireEvent.click(screen.getByRole("button", { name: "Approve & run" }));
    await waitFor(() => expect(screen.queryByTestId("approval-submit-error")).toBeNull());
    release(new Response(JSON.stringify({ status: "rejected" }), { status: 200 }));
  });
});

describe("the advanced-model nudge keeps its choices when the POST fails", () => {
  it("stays pending with an inline error and enabled buttons", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("", { status: 502 })));
    const onResolved = renderCard({
      tool: "suggest_advanced_model",
      summary: { reason: "Long refactor." },
    });

    fireEvent.click(screen.getByRole("button", { name: "Switch & retry" }));
    const error = await screen.findByTestId("approval-submit-error");
    expect(error).toHaveTextContent("HTTP 502");
    expect(onResolved).not.toHaveBeenCalled();
    // Pending copy, not "Suggestion failed"; every choice still offered.
    expect(screen.getByText(/for the rest of this chat\?/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Switch & retry" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Just switch" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Dismiss" })).toBeEnabled();
  });

  it("resolves on the server's answer once a retry succeeds", async () => {
    const fetchMock = vi
      .fn()
      .mockRejectedValueOnce(new TypeError("Failed to fetch"))
      .mockResolvedValueOnce(
        new Response(JSON.stringify({ status: "rejected" }), { status: 200 }),
      );
    vi.stubGlobal("fetch", fetchMock);
    const onResolved = renderCard({ tool: "suggest_advanced_model", summary: {} });

    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    await screen.findByTestId("approval-submit-error");
    expect(onResolved).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    await waitFor(() => expect(onResolved).toHaveBeenCalledTimes(1));
    expect(onResolved.mock.calls[0][0]).toMatchObject({ status: "rejected" });
  });
});
