import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { ApprovalCard } from "./ApprovalCards";
import type { Approval } from "./history";

// An approval card outlives the turn that staged it, and execution reopens that
// turn's credential seat (#167). When the turn was running on a named account,
// the card has to say so before the user clicks Send — otherwise "Send" is a
// blind choice between clients.

function renderCard(approval: Partial<Approval>) {
  const full: Approval = {
    id: "ap_1",
    tool: "mcp_send_grid_send_email",
    status: "pending",
    summary: { subject: "Q3 numbers", content: "body", content_type: "text/plain" },
    ...approval,
  } as Approval;
  render(<ApprovalCard approval={full} conversationId="conv_1" onResolved={() => {}} />);
}

describe("the approval card's credential seat", () => {
  it("names the account a named-seat send will run as", () => {
    renderCard({ mcpServer: "send_grid", mcpAccount: "client_a" });
    const seat = screen.getByTestId("approval-seat");
    expect(seat.textContent).toContain("client_a");
    expect(seat.textContent).toContain("send_grid");
  });

  it("stays silent on the default seat — there is no ambiguity to resolve", () => {
    renderCard({ mcpServer: "send_grid" });
    expect(screen.queryByTestId("approval-seat")).toBeNull();
  });

  it("stays silent for a card with no recorded seat (native tool / legacy row)", () => {
    renderCard({});
    expect(screen.queryByTestId("approval-seat")).toBeNull();
  });
});
