import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { ApprovalCard } from "./ApprovalCards";
import type { Approval } from "./history";

// A task scheduled from chat inherits this conversation's connectors
// (ADR-0068). The card is the one moment a human sees what the task will be
// able to reach before it exists — so it names the connectors, and it warns
// when there are none rather than letting the first run fail silently.

function renderSchedule(summary: Approval["summary"]) {
  const approval: Approval = {
    id: "ap_sched",
    tool: "schedule_task",
    status: "pending",
    summary: { tool: "schedule_task", name: "Daily health scan", prompt_preview: "scan", recurring: true, cron: "0 9 * * *", ...summary },
  } as Approval;
  render(<ApprovalCard approval={approval} conversationId="conv_1" onResolved={() => {}} />);
}

describe("the schedule card's connectors", () => {
  it("names the connectors the task inherits from the chat", () => {
    renderSchedule({ connectors: ["email", "magnite_mcp (reklaim)"], always_on_connectors: [], no_connectors: false });
    const line = screen.getByTestId("schedule-connectors");
    expect(line.textContent).toContain("email");
    expect(line.textContent).toContain("magnite_mcp (reklaim)");
    expect(screen.queryByTestId("schedule-no-connectors")).toBeNull();
  });

  it("warns before Approve when the task would run with no connectors at all", () => {
    renderSchedule({ connectors: [], always_on_connectors: [], no_connectors: true });
    const warning = screen.getByTestId("schedule-no-connectors");
    expect(warning.textContent).toContain("No connectors");
    expect(warning.textContent).toContain("Operations Center");
    expect(screen.queryByTestId("schedule-connectors")).toBeNull();
  });

  it("says 'always-on only' when nothing was selected but the bundle binds servers anyway", () => {
    renderSchedule({ connectors: [], always_on_connectors: ["mailbox"], no_connectors: false });
    expect(screen.getByTestId("schedule-connectors").textContent).toContain("always-on only (mailbox)");
    expect(screen.queryByTestId("schedule-no-connectors")).toBeNull();
  });

  it("treats a legacy card with no snapshot as the no-connector case it really is", () => {
    renderSchedule({});
    expect(screen.getByTestId("schedule-no-connectors")).toBeTruthy();
  });
});
