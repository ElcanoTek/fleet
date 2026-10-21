import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";

import { ApprovalCard, actionLabel, askAgainPrompt } from "./ApprovalCards";
import { applyAllText, approvalToolLabel, connectorNames, ConnectorNamesContext } from "./approvalToolLabel";
import type { Approval } from "./history";

// A trader reads the approval card, so its batch-approval checkbox names the
// tool in words ("every Index Exchange “Execute plan” request"), never the
// model-facing id ("mcp_ix_execute_plan"): the bundle's connector name when
// the chat knows it, else fleet's SSP names, else the humanised id.

afterEach(cleanup);

describe("approvalToolLabel", () => {
  const names = connectorNames([
    { name: "ix", display_name: "Index Exchange" },
    { name: "deal_sheet", display_name: "Deal Sheet" },
    { name: "blank", display_name: "  " },
  ]);

  it.each([
    // [tool, mcpServer, connector, action]
    ["mcp_ix_execute_plan", "ix", "Index Exchange", "Execute plan"],
    // No mcpServer on the row: split on the first underscore.
    ["mcp_ix_execute_plan", undefined, "Index Exchange", "Execute plan"],
    // A connector id with an underscore splits exactly with mcpServer.
    ["mcp_deal_sheet_next_campaign_id", "deal_sheet", "Deal Sheet", "Next campaign id"],
    // fleet's SSP names, and the short prefix their tools repeat.
    ["mcp_indexexchange_ix_create_deal", "indexexchange", "Index Exchange", "Create deal"],
    ["mcp_magnite_magnite_update_deal", "magnite", "Magnite", "Update deal"],
    // A connector's short prefix is dropped even outside fleet's SSP list.
    ["mcp_openx_ox_create_deal", "openx", "Openx", "Create deal"],
    ["mcp_triplelift_tl_update_deal", "triplelift", "Triplelift", "Update deal"],
    // An older row with no mcpServer: the longest connector the chat knows.
    ["mcp_deal_sheet_next_campaign_id", undefined, "Deal Sheet", "Next campaign id"],
    // Unknown connector, blank display name: the humanised id.
    ["mcp_blank_publish_page", "blank", "Blank", "Publish page"],
    ["mcp_pages_deploy_page", undefined, "Pages", "Deploy page"],
  ])("%s (server %s) → %s “%s”", (tool, server, connector, action) => {
    expect(approvalToolLabel(tool, server, names)).toEqual({ connector, action });
  });

  // Production bundles name SSP servers with the `_mcp` suffix
  // (elcano-config's magnite_mcp, indexexchange_mcp, …), and their tools
  // repeat the connector after it (Codex review, #1828).
  it.each([
    // [tool, mcpServer, connector, action]
    ["mcp_magnite_mcp_magnite_update_deal", "magnite_mcp", "Magnite", "Update deal"],
    ["mcp_indexexchange_mcp_ix_create_deal", "indexexchange_mcp", "Index Exchange", "Create deal"],
    ["mcp_pubmatic_mcp_pm_update_deal", "pubmatic_mcp", "PubMatic", "Update deal"],
    ["mcp_openx_mcp_ox_create_deal", "openx_mcp", "Openx", "Create deal"],
    ["mcp_equativ_mcp_eq_create_deal", "equativ_mcp", "Equativ", "Create deal"],
    ["mcp_xandr_mcp_create_deal", "xandr_mcp", "Xandr", "Create deal"],
    // No mcpServer on the row and no catalog: the suffix stays with the connector.
    ["mcp_magnite_mcp_magnite_update_deal", undefined, "Magnite", "Update deal"],
    ["mcp_triplelift_mcp_tl_update_deal", undefined, "Triplelift", "Update deal"],
  ])("a _mcp server: %s (server %s) → %s “%s”", (tool, server, connector, action) => {
    expect(approvalToolLabel(tool, server)).toEqual({ connector, action });
  });

  // A named seat registers its tools as mcp_<server>_<account>_<tool>
  // (internal/agentcore.RegisteredMCPName); the seat is not the action
  // (Codex review, #1828).
  it.each([
    // [tool, mcpServer, mcpAccount, connector, action]
    ["mcp_send_grid_client_a_send_email", "send_grid", "client_a", "Send grid", "Send email"],
    // The account folds the way fleet registers it: client-a is client_a.
    ["mcp_send_grid_client_a_send_email", "send_grid", "client-a", "Send grid", "Send email"],
    ["mcp_magnite_mcp_omnicom_magnite_update_deal", "magnite_mcp", "omnicom", "Magnite", "Update deal"],
    ["mcp_indexexchange_mcp_rain_barrel_ix_create_deal", "indexexchange_mcp", "rain barrel", "Index Exchange", "Create deal"],
    // The default seat's tools carry no account.
    ["mcp_send_grid_send_email", "send_grid", undefined, "Send grid", "Send email"],
    ["mcp_send_grid_send_email", "send_grid", "", "Send grid", "Send email"],
  ])("a named seat: %s (%s as %s) → %s “%s”", (tool, server, account, connector, action) => {
    expect(approvalToolLabel(tool, server, undefined, account)).toEqual({ connector, action });
  });

  it("names a native tool by its action alone", () => {
    expect(approvalToolLabel("send_email", undefined, names)).toEqual({ action: "Send email" });
    expect(approvalToolLabel("mcp_", undefined, names).action).not.toMatch(/mcp/i);
  });

  it("never puts a raw tool id in the checkbox text", () => {
    for (const tool of ["mcp_ix_execute_plan", "mcp_deal_sheet_next_campaign_id", "mcp_x", "send_email"]) {
      const text = applyAllText(approvalToolLabel(tool, undefined, names));
      expect(text).not.toMatch(/mcp_|_/);
      expect(text).toMatch(/^Apply my choice to every .+ request in this chat$/);
    }
    expect(applyAllText({ connector: "Index Exchange", action: "Execute plan" })).toBe(
      "Apply my choice to every Index Exchange “Execute plan” request in this chat",
    );
  });
});

function renderCard(approval: Partial<Approval>, names?: Map<string, string>) {
  const full = { id: "ap_1", status: "pending", summary: {}, tool: "mcp_ix_execute_plan", ...approval } as Approval;
  const card = <ApprovalCard approval={full} conversationId="conv_1" onResolved={vi.fn()} />;
  render(names ? <ConnectorNamesContext.Provider value={names}>{card}</ConnectorNamesContext.Provider> : card);
}

describe("the approval card's words for the tool", () => {
  const names = new Map([["ix", "Index Exchange"]]);

  it("a readable (describer) card's checkbox names the connector and action", () => {
    renderCard(
      {
        mcpServer: "ix",
        mcpAccount: "elcano",
        summary: { tool: "mcp_ix_execute_plan", args: [{ key: "plan_id", value: "dp_1" }] },
        card: { title: "Create 1 Index Exchange deal for Elcano", items: [{ label: "ELC_NW_Q4_IX_Display_US" }] },
      },
      names,
    );
    const label = screen.getByTestId("approval-apply-all").closest("label");
    expect(label?.textContent).toBe("Apply my choice to every Index Exchange “Execute plan” request in this chat");
    // The seat line names the connector too.
    expect(screen.getByTestId("approval-seat").textContent).toBe("Runs as elcano on Index Exchange");
  });

  it("the generic card's checkbox has no raw id, even outside the chat's names", () => {
    renderCard({ summary: { tool: "mcp_ix_execute_plan", args: [{ key: "plan_id", value: "dp_1" }] } });
    const label = screen.getByTestId("approval-apply-all").closest("label");
    expect(label?.textContent).toBe("Apply my choice to every Ix “Execute plan” request in this chat");
  });

  it("the email card's checkbox names the action", () => {
    renderCard({
      tool: "send_email",
      summary: { to: "a@example.com", subject: "Report", content: "<p>hi</p>", content_type: "text/html" },
    });
    const label = screen.getByTestId("approval-apply-all").closest("label");
    expect(label?.textContent).toBe("Apply my choice to every “Send email” request in this chat");
  });

  it("the generic card's title names the action the way its checkbox does", () => {
    renderCard({
      tool: "mcp_indexexchange_ix_create_deal",
      mcpServer: "indexexchange",
      summary: { tool: "mcp_indexexchange_ix_create_deal", args: [{ key: "deal_name", value: "Q4" }] },
    });
    expect(screen.getByText('ACTION REQUIRED · Run "Create deal"?')).toBeTruthy();
    const label = screen.getByTestId("approval-apply-all").closest("label");
    expect(label?.textContent).toBe("Apply my choice to every Index Exchange “Create deal” request in this chat");
  });

  // An older row with no mcpServer splits its id with the chat's connector
  // names everywhere, not only in the checkbox (Codex review, #1828).
  it("an older row's title, checkbox and Ask again split the id with the chat's connectors", () => {
    const sheet = new Map([["deal_sheet", "Deal Sheet"]]);
    renderCard(
      {
        tool: "mcp_deal_sheet_next_campaign_id",
        summary: { tool: "mcp_deal_sheet_next_campaign_id", args: [{ key: "client", value: "Elcano" }] },
      },
      sheet,
    );
    expect(screen.getByText('ACTION REQUIRED · Run "Next campaign id"?')).toBeTruthy();
    const label = screen.getByTestId("approval-apply-all").closest("label");
    expect(label?.textContent).toBe("Apply my choice to every Deal Sheet “Next campaign id” request in this chat");
    expect(actionLabel("mcp_deal_sheet_next_campaign_id", undefined, undefined, sheet)).toEqual({
      action: "Next campaign id",
      server: "deal_sheet",
    });
    const prompt = askAgainPrompt(
      { id: "ap_1", status: "rejected", summary: {}, tool: "mcp_deal_sheet_next_campaign_id" } as Approval,
      sheet,
    );
    expect(prompt).toContain("next campaign id (mcp_deal_sheet_next_campaign_id)");
  });

  it("a named seat's card names the action without the seat, in its title and checkbox", () => {
    renderCard({
      tool: "mcp_send_grid_client_a_send_email",
      mcpServer: "send_grid",
      mcpAccount: "client_a",
      summary: { tool: "mcp_send_grid_client_a_send_email", args: [{ key: "to", value: "a@example.com" }] },
    });
    const label = screen.getByTestId("approval-apply-all").closest("label");
    expect(label?.textContent).toBe("Apply my choice to every Send grid “Send email” request in this chat");
    expect(label?.textContent).not.toMatch(/client/i);
  });

  it("a production _mcp server's card names the SSP and the action", () => {
    renderCard({
      tool: "mcp_magnite_mcp_magnite_update_deal",
      mcpServer: "magnite_mcp",
      mcpAccount: "elcano",
      summary: { tool: "mcp_magnite_mcp_magnite_update_deal", args: [{ key: "deal_id", value: "1" }] },
    });
    expect(screen.getByText('ACTION REQUIRED · Run "Update deal"?')).toBeTruthy();
    const label = screen.getByTestId("approval-apply-all").closest("label");
    expect(label?.textContent).toBe("Apply my choice to every Magnite “Update deal” request in this chat");
    expect(screen.getByTestId("approval-seat").textContent).toBe("Runs as elcano on Magnite");
  });

  it("the seat line uses fleet's SSP name when the chat has none", () => {
    renderCard({ tool: "mcp_indexexchange_ix_create_deal", mcpServer: "indexexchange", mcpAccount: "elcano", summary: { args: [] } });
    expect(screen.getByTestId("approval-seat").textContent).toBe("Runs as elcano on Index Exchange");
  });

  it("without a known name the seat line keeps the connector id", () => {
    renderCard({ mcpServer: "ix", mcpAccount: "elcano", summary: { tool: "mcp_ix_execute_plan", args: [] } });
    expect(screen.getByTestId("approval-seat").textContent).toBe("Runs as elcano on ix");
  });
});
