import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ApprovalCard } from "./ApprovalCards";
import type { Approval } from "./history";

// A bundle can list a critical tool in
// agent_policy.critical_tool_no_session_approval so every call needs its own
// decision. The server marks those cards no_session_approval; the card then
// withholds "Apply my choice to all … calls in this chat" and always posts
// scope "once". Every other card keeps the checkbox (#300).

function renderCard(approval: Partial<Approval> & Pick<Approval, "tool">) {
  const full: Approval = { id: "ap_1", status: "pending", summary: {}, ...approval } as Approval;
  render(<ApprovalCard approval={full} conversationId="conv_1" onResolved={vi.fn()} />);
}

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("cards for a tool that needs a decision per call", () => {
  it("hide apply-all on the generic card and post scope once", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ status: "approved", result_text: "ok", is_err: false }), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);
    renderCard({
      tool: "mcp_deals_create_deal",
      noSessionApproval: true,
      summary: { tool: "mcp_deals_create_deal", args: [{ key: "deal_name", value: "Q4" }] },
    });
    expect(screen.queryByTestId("approval-apply-all")).toBeNull();
    expect(screen.queryByText(/Apply my choice to all/)).toBeNull();
    // Approve and Cancel are unchanged.
    expect(screen.getByRole("button", { name: "Cancel" })).toBeEnabled();
    fireEvent.click(screen.getByRole("button", { name: "Approve & run" }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    const body = JSON.parse(String(fetchMock.mock.calls[0][1]?.body));
    expect(body).toMatchObject({ approved: true, scope: "once" });
  });

  it("hide apply-all on the email card", () => {
    renderCard({
      tool: "mcp_sendgrid_send_email",
      noSessionApproval: true,
      summary: { to: "a@example.com", subject: "Report", content: "<p>hi</p>", content_type: "text/html" },
    });
    expect(screen.getByRole("button", { name: "Send" })).toBeTruthy();
    expect(screen.queryByTestId("approval-apply-all")).toBeNull();
  });

  it("keep apply-all, and its session scope, for every other tool", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ status: "approved", result_text: "ok", is_err: false }), { status: 200 }),
    );
    vi.stubGlobal("fetch", fetchMock);
    renderCard({
      tool: "mcp_pages_deploy_page",
      summary: { tool: "mcp_pages_deploy_page", args: [{ key: "slug", value: "q3" }] },
    });
    fireEvent.click(screen.getByTestId("approval-apply-all"));
    fireEvent.click(screen.getByRole("button", { name: "Approve + allow all" }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toMatchObject({ scope: "session" });
  });
});
