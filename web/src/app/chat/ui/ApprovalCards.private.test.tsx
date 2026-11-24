import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ApprovalCard } from "./ApprovalCards";
import type { Approval } from "./history";

// Every email the bundle's sender sends is copied to the team Outbox, which
// the whole organization reads. An outbound email card (private_option)
// offers a Private box that keeps this one out: ticked to start when the
// model passed private, and the person's choice goes with Send either way.

const emailSummary = { to: "alice@client.example", subject: "Weekly numbers", content: "<p>hi</p>", content_type: "text/html" };

function renderCard(approval: Partial<Approval>, onResolved = vi.fn()) {
  const full: Approval = {
    id: "ap_1",
    tool: "mcp_ses_outbound_send_email",
    status: "pending",
    summary: emailSummary,
    ...approval,
  } as Approval;
  render(<ApprovalCard approval={full} conversationId="conv_1" onResolved={onResolved} />);
  return onResolved;
}

function stubApproved(extra: Record<string, unknown> = {}) {
  const fetchMock = vi.fn().mockResolvedValue(
    new Response(JSON.stringify({ status: "approved", result_text: "queued", is_err: false, ...extra }), { status: 200 }),
  );
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

function postedBody(fetchMock: ReturnType<typeof vi.fn>) {
  return JSON.parse(String(fetchMock.mock.calls[0][1]?.body));
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("the email card's Private box", () => {
  it("starts unticked and sends private false with Send", async () => {
    const fetchMock = stubApproved();
    renderCard({ privateOption: true });
    const box = screen.getByTestId("approval-private") as HTMLInputElement;
    expect(box.checked).toBe(false);
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(postedBody(fetchMock)).toMatchObject({ approved: true, private: false });
  });

  it("starts ticked when the model passed private, and the person can untick it", async () => {
    const fetchMock = stubApproved({ private_choice: false });
    const onResolved = renderCard({ privateOption: true, summary: { ...emailSummary, private: true } });
    const box = screen.getByTestId("approval-private") as HTMLInputElement;
    expect(box.checked).toBe(true);
    fireEvent.click(box);
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(postedBody(fetchMock)).toMatchObject({ private: false });
    await waitFor(() => expect(onResolved).toHaveBeenCalled());
    expect(onResolved.mock.calls[0][0]).toMatchObject({ status: "approved", privateChoice: false });
  });

  it("sends private true when ticked, and records the choice", async () => {
    const fetchMock = stubApproved({ private_choice: true });
    const onResolved = renderCard({ privateOption: true });
    fireEvent.click(screen.getByTestId("approval-private"));
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(postedBody(fetchMock)).toMatchObject({ approved: true, private: true });
    await waitFor(() => expect(onResolved).toHaveBeenCalled());
    expect(onResolved.mock.calls[0][0]).toMatchObject({ privateChoice: true });
  });

  it("never sends a choice with Cancel", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ status: "rejected" }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    renderCard({ privateOption: true });
    fireEvent.click(screen.getByTestId("approval-private"));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(postedBody(fetchMock)).not.toHaveProperty("private");
  });

  it("is not offered, and nothing is sent, without private_option", async () => {
    const fetchMock = stubApproved();
    renderCard({});
    expect(screen.queryByTestId("approval-private")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(postedBody(fetchMock)).not.toHaveProperty("private");
  });

  it("says a settled private email was not saved to the Outbox", () => {
    renderCard({ status: "approved", privateChoice: true, resultText: "queued" });
    expect(screen.getByTestId("email-private-settled").textContent).toBe("Private: not saved to the Outbox");
    cleanup();
    renderCard({ status: "approved", privateChoice: false, resultText: "queued" });
    expect(screen.queryByTestId("email-private-settled")).toBeNull();
  });
});
