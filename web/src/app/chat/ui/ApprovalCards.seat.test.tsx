import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { ApprovalCard } from "./ApprovalCards";
import { DefaultAccountLabelsContext, defaultAccountLabels } from "./approvalToolLabel";
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

  // A chat that runs the connector as several accounts
  // (docs/MULTI-ACCOUNT-CHATS.md): the default account's card is named too,
  // or it would be the one card the user has to guess about.
  it("names the default account when the chat runs the connector as several", () => {
    const full = {
      id: "ap_1",
      tool: "mcp_send_grid_send_email",
      status: "pending",
      summary: { subject: "Q3 numbers", content: "body", content_type: "text/plain" },
      mcpServer: "send_grid",
    } as Approval;
    render(
      <DefaultAccountLabelsContext.Provider value={new Map([["send_grid", "Elcano"]])}>
        <ApprovalCard approval={full} conversationId="conv_1" onResolved={() => {}} />
      </DefaultAccountLabelsContext.Provider>,
    );
    expect(screen.getByTestId("approval-seat").textContent).toBe("Sending as Elcano (default account) on send_grid");
  });

  it("stays silent for a card with no recorded seat (native tool / legacy row)", () => {
    renderCard({});
    expect(screen.queryByTestId("approval-seat")).toBeNull();
  });

  it("lists only connectors that are on and run as several accounts", () => {
    const labels = defaultAccountLabels(
      [
        { name: "ix", enabled: true, selected_accounts: ["", "twc"] },
        { name: "ox", enabled: true },
        { name: "pm", enabled: false, selected_accounts: ["", "twc"] },
      ],
      "Elcano",
    );
    expect([...labels]).toEqual([["ix", "Elcano"]]);
  });
});
