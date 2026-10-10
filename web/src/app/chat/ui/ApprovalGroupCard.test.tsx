import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ApprovalGroupCard } from "./ApprovalGroupCard";
import type { Approval } from "./history";

// The grouped approval card (docs/GROUPED-APPROVALS.md): one card for the
// calls one turn staged, a checkbox per call, Approve all (k) / One at a time
// / Cancel all, one POST to the group endpoint.

const readable = {
  title: "Create 12 deals on PubMatic",
  items: [{ label: "Q4 Video", id: "PM-1" }],
};

function members(): Approval[] {
  return [
    {
      id: "ap-pm",
      tool: "mcp_pubmatic_execute_plan",
      summary: { tool: "mcp_pubmatic_execute_plan", args: [{ key: "plan_id", value: "p1" }] },
      status: "pending",
      groupId: "11111111-2222-3333-4444-555555555555",
      card: readable,
      mcpServer: "pubmatic",
      mcpAccount: "client_a",
      expiresAt: Math.floor(Date.now() / 1000) + 900,
    },
    {
      id: "ap-mg",
      tool: "mcp_magnite_execute_plan",
      summary: { tool: "mcp_magnite_execute_plan", args: [{ key: "plan_id", value: "p1" }] },
      status: "pending",
      groupId: "11111111-2222-3333-4444-555555555555",
      expiresAt: Math.floor(Date.now() / 1000) + 600,
    },
  ];
}

function renderGroup(overrides: Partial<Parameters<typeof ApprovalGroupCard>[0]> = {}) {
  const props = {
    groupId: "11111111-2222-3333-4444-555555555555",
    approvals: members(),
    conversationId: "conv_1",
    onResolved: vi.fn(),
    onResumeExpected: vi.fn(),
    onOneAtATime: vi.fn(),
    ...overrides,
  };
  render(<ApprovalGroupCard {...props} />);
  return props;
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("ApprovalGroupCard", () => {
  it("lists every call with its readable card or arguments, its seat, and one countdown", async () => {
    renderGroup();
    expect(screen.getByTestId("approval-group-title").textContent).toBe("2 actions to approve");
    const rows = screen.getAllByTestId("approval-group-row");
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toContain("Create 12 deals on PubMatic");
    expect(rows[0].textContent).toContain("Q4 Video");
    expect(rows[0].textContent).toContain("Runs as client_a on pubmatic");
    expect(rows[1].textContent).toContain('Run "Execute plan"');
    expect(rows[1].textContent).toContain("plan_id");
    // One countdown, on the earliest deadline (10 minutes, not 15).
    const countdowns = await screen.findAllByTestId("approval-countdown");
    expect(countdowns).toHaveLength(1);
    expect(countdowns[0].textContent).toMatch(/(9|10):\d\d/);
    expect(screen.getByTestId("approval-group-approve").textContent).toBe("Approve all (2)");
  });

  it("approves the checked calls and declines the unchecked one in one POST", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          group_id: "11111111-2222-3333-4444-555555555555",
          resume: true,
          results: [
            { approval_id: "ap-pm", decision: "approve", status_code: 200, result: { status: "approved", executing: true, result_text: "Approved — executing…", resume: true } },
            { approval_id: "ap-mg", decision: "decline", status_code: 200, result: { status: "rejected" } },
          ],
        }),
        { status: 200 },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);
    const props = renderGroup();

    fireEvent.click(screen.getAllByTestId("approval-group-check")[1]);
    expect(screen.getByTestId("approval-group-approve").textContent).toBe("Approve all (1)");
    expect(screen.getByTestId("approval-group-unchecked-note").textContent).toContain("declined");
    fireEvent.click(screen.getByTestId("approval-group-approve"));

    await waitFor(() => expect(props.onResolved).toHaveBeenCalledTimes(2));
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe("/api/conversations/conv_1/approval-groups/11111111-2222-3333-4444-555555555555");
    expect(JSON.parse(String(init?.body))).toEqual({ approve: ["ap-pm"], decline: ["ap-mg"] });
    expect(props.onResolved).toHaveBeenCalledWith(expect.objectContaining({ id: "ap-pm", status: "pending", executing: true }));
    expect(props.onResolved).toHaveBeenCalledWith(expect.objectContaining({ id: "ap-mg", status: "rejected" }));
    expect(props.onResumeExpected).toHaveBeenCalledWith(true);
  });

  it("Cancel all declines every call", async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ results: [] }), { status: 200 }));
    vi.stubGlobal("fetch", fetchMock);
    renderGroup();
    fireEvent.click(screen.getByTestId("approval-group-cancel"));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({ approve: [], decline: ["ap-pm", "ap-mg"] });
  });

  it("One at a time hands back to the individual cards without a request", () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    const props = renderGroup();
    fireEvent.click(screen.getByTestId("approval-group-one-at-a-time"));
    expect(props.onOneAtATime).toHaveBeenCalledTimes(1);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("disables Approve all with nothing checked", () => {
    renderGroup();
    for (const box of screen.getAllByTestId("approval-group-check")) fireEvent.click(box);
    expect(screen.getByTestId("approval-group-approve")).toBeDisabled();
    expect(screen.getByTestId("approval-group-cancel")).toBeEnabled();
  });

  it("keeps every card pending when the server refuses the decision, and names a call it could not decide", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(new Response("approval \"x\" is not in this approval group", { status: 409 }))
      .mockResolvedValueOnce(
        new Response(
          JSON.stringify({
            results: [
              { approval_id: "ap-pm", decision: "approve", status_code: 503, error: "fleet is shutting down; the action was not run." },
              { approval_id: "ap-mg", decision: "approve", status_code: 200, result: { status: "approved", is_err: false, result_text: "ok" } },
            ],
          }),
          { status: 200 },
        ),
      );
    vi.stubGlobal("fetch", fetchMock);
    const props = renderGroup();

    fireEvent.click(screen.getByTestId("approval-group-approve"));
    await waitFor(() => expect(screen.getByTestId("approval-submit-error").textContent).toContain("HTTP 409"));
    expect(props.onResolved).not.toHaveBeenCalled();

    fireEvent.click(screen.getByTestId("approval-group-approve"));
    await waitFor(() => expect(screen.getByTestId("approval-submit-error").textContent).toContain("One action was not decided"));
    expect(props.onResolved).toHaveBeenCalledTimes(1);
    expect(props.onResolved).toHaveBeenCalledWith(expect.objectContaining({ id: "ap-mg", status: "approved" }));
  });
});
