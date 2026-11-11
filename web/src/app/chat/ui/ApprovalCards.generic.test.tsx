import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { ApprovalCard } from "./ApprovalCards";
import type { Approval } from "./history";

// Non-email critical tools (bundle-declared suffixes like a pages deploy) used
// to fall through to the email-shaped card: pending said "Send this email?",
// resolved said "Email sent ✓" — for a page publish. These tests pin the
// generic action card that replaced the fallthrough, and that email tools
// still get the email card.

function renderCard(
  approval: Partial<Approval> & Pick<Approval, "tool">,
  onAskAgain?: (a: Approval) => void,
) {
  const full: Approval = {
    id: "ap_1",
    status: "pending",
    summary: {},
    ...approval,
  } as Approval;
  render(
    <ApprovalCard
      approval={full}
      conversationId="conv_1"
      onResolved={() => {}}
      onAskAgain={onAskAgain}
    />,
  );
}

describe("the generic critical-action card", () => {
  it("renders a pending pages deploy with honest copy and its arguments", () => {
    renderCard({
      tool: "mcp_pages_deploy_page",
      summary: {
        tool: "mcp_pages_deploy_page",
        args: [
          { key: "publish", value: "true" },
          { key: "slug", value: "q3-report" },
        ],
      },
    });
    expect(screen.getByTestId("generic-action-card")).toBeTruthy();
    expect(screen.getByText(/ACTION REQUIRED · Run "Deploy page"\?/)).toBeTruthy();
    // Never the email card's copy.
    expect(screen.queryByText(/send this email/i)).toBeNull();
    // The server half of the header names the tool and its server.
    expect(screen.getByText("pages")).toBeTruthy();
    expect(screen.getByText("mcp_pages_deploy_page")).toBeTruthy();
    // Arguments verbatim.
    expect(screen.getByText("slug:")).toBeTruthy();
    expect(screen.getByText("q3-report")).toBeTruthy();
    // Honest verbs on the buttons.
    expect(screen.getByRole("button", { name: "Approve & run" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeTruthy();
    // Batch approval stays available (#300), labeled with the bare tool name.
    expect(screen.getByTestId("approval-apply-all")).toBeTruthy();
  });

  it("renders a notify-mode record as information, not a past approval", () => {
    renderCard({
      tool: "mcp_pages_deploy_page",
      status: "approved",
      recorded: true,
      resultText:
        "Ran without asking: this tool is declared notify-mode in the client bundle. Undo with mcp_pages_rollback_page(slug, version_id).",
      summary: { tool: "mcp_pages_deploy_page", args: [{ key: "slug", value: "q3-report" }] },
    });
    expect(screen.getByText(/Deploy page · ran without asking/)).toBeTruthy();
    expect(screen.queryByText(/email sent/i)).toBeNull();
    // The undo hint — the whole case for running without asking — is on the card.
    expect(screen.getByTestId("approval-result").textContent).toContain("mcp_pages_rollback_page");
    // A record has no decision to make.
    expect(screen.queryByRole("button", { name: "Approve & run" })).toBeNull();
  });

  it("renders resolved outcomes with action verbs", () => {
    renderCard({
      tool: "mcp_pages_deploy_page",
      status: "approved",
      resultText: `{"version":{"id":"143"}}`,
      summary: { tool: "mcp_pages_deploy_page" },
    });
    expect(screen.getByText(/Deploy page · completed ✓/)).toBeTruthy();
  });

  it("offers Ask again on a timed-out card and submits the re-stage prompt", () => {
    const onAskAgain = vi.fn();
    renderCard(
      {
        tool: "mcp_pages_deploy_page",
        status: "rejected",
        resultText: "Approval timed out — auto-denied. The action was not taken.",
        summary: { tool: "mcp_pages_deploy_page" },
      },
      onAskAgain,
    );
    expect(screen.getByText(/Deploy page · timed out/)).toBeTruthy();
    fireEvent.click(screen.getByTestId("approval-ask-again"));
    expect(onAskAgain).toHaveBeenCalledTimes(1);
    expect(onAskAgain.mock.calls[0][0].tool).toBe("mcp_pages_deploy_page");
  });

  it("does not offer Ask again on a user-declined card", () => {
    renderCard({
      tool: "mcp_pages_deploy_page",
      status: "rejected",
      resultText: "User declined this action.",
      summary: { tool: "mcp_pages_deploy_page" },
    });
    expect(screen.getByText(/Deploy page · cancelled/)).toBeTruthy();
    expect(screen.queryByTestId("approval-ask-again")).toBeNull();
  });

  it("keeps email tools on the email card", () => {
    renderCard({
      tool: "mcp_sendgrid_send_email",
      summary: { subject: "Weekly report", content: "<p>hi</p>", content_type: "text/html" },
    });
    expect(screen.getByText(/Send this email\?/)).toBeTruthy();
    expect(screen.queryByTestId("generic-action-card")).toBeNull();
  });

  it("keeps a described call's own arguments under Details", () => {
    renderCard({
      tool: "connector_studio",
      summary: {
        tool: "connector_studio",
        args: [
          { key: "What", value: "Run these steps in your live browser session" },
          { key: "Step 1", value: "Type the password into #pw" },
        ],
        raw_args: [
          { key: "action", value: "probe_browser" },
          { key: "password_ref", value: "$secret:password" },
          { key: "steps[1]", value: '{"action":"fill","selector":"#pw","value":"{{PASSWORD}}"}' },
        ],
      },
    });
    expect(screen.getByText("Type the password into #pw")).toBeTruthy();
    const details = screen.getByTestId("approval-card-details");
    expect(details.tagName).toBe("DETAILS");
    expect((details as HTMLDetailsElement).open).toBe(false);
    const raw = screen.getByTestId("approval-raw-args");
    expect(details.contains(raw)).toBe(true);
    expect(raw.textContent).toContain("password_ref: $secret:password");
    expect(raw.textContent).toContain("action: probe_browser");
    expect(raw.textContent).toContain('steps[1]: {"action":"fill","selector":"#pw"');
  });

  it("keeps Details on a settled and on a running described card", () => {
    const summary = {
      tool: "playwright_browser",
      args: [{ key: "What", value: "Run these steps in a new headless browser" }],
      raw_args: [{ key: "action", value: "run" }],
    };
    renderCard({ tool: "playwright_browser", status: "approved", resultText: "ok", summary });
    expect(screen.getByTestId("approval-raw-args").textContent).toContain("action: run");
    document.body.innerHTML = "";
    renderCard({ tool: "playwright_browser", status: "pending", executing: true, summary });
    expect(screen.getByTestId("approval-raw-args").textContent).toContain("action: run");
  });

  it("has no Details for a card without raw arguments", () => {
    renderCard({
      tool: "mcp_pages_deploy_page",
      summary: { tool: "mcp_pages_deploy_page", args: [{ key: "slug", value: "q3-report" }] },
    });
    expect(screen.queryByTestId("approval-card-details")).toBeNull();
  });

  it("falls back to raw args when the input was unparseable", () => {
    renderCard({
      tool: "mcp_pages_deploy_page",
      summary: { tool: "mcp_pages_deploy_page", raw: "{not json" },
    });
    expect(screen.getByText("{not json")).toBeTruthy();
  });
});
