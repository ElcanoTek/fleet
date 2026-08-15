import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { ToolChip } from "./ToolChips";
import type { SubagentActivity, ToolCall } from "./history";

// Sub-agent visibility in the transcript (#1043 follow-up). The reported
// symptom was "I just see its initial instructions in the spinning … animation
// which is not helpful": the spawn chip rendered the task text and nothing
// else for the child's whole run. These tests pin what the chip shows now.

const activity = (over: Partial<SubagentActivity> = {}): SubagentActivity => ({
  childId: "subagent-abc12345-0000-0000-0000-000000000000",
  role: "explore",
  phase: "tool",
  steps: 2,
  toolsUsed: ["outlook_email_search"],
  trail: [
    { phase: "tool", tool: "outlook_email_search", detail: "query=is:unread" },
    { phase: "tool_result", tool: "outlook_email_search", detail: "3 messages" },
  ],
  current: "outlook_email_search · query=is:unread",
  ...over,
});

const spawnCall = (over: Partial<ToolCall> = {}): ToolCall => ({
  id: "call_1",
  name: "spawn_subagent",
  input: JSON.stringify({ task: "Summarize the last 3 emails", role: "explore" }),
  state: "pending",
  ...over,
});

describe("ToolChip: live sub-agent activity", () => {
  it("opens by default and shows what the child is doing", () => {
    render(<ToolChip tc={spawnCall({ subagent: activity() })} taskTrackerDisplay={null} />);
    // No click: a delegation runs for minutes, so its panel is open by default.
    const panel = screen.getByTestId("chat-subagent-activity");
    expect(panel).toBeTruthy();
    expect(panel.textContent).toContain("outlook_email_search");
    expect(panel.textContent).toContain("query=is:unread");
    expect(panel.textContent).toContain("2 steps");
    expect(panel.textContent).toContain("running…");
    // The task is still shown — the child's brief is context for its steps.
    expect(screen.getByText(/Summarize the last 3 emails/)).toBeTruthy();
  });

  it("shows the child's role and step count on the collapsed pill", () => {
    render(<ToolChip tc={spawnCall({ subagent: activity() })} taskTrackerDisplay={null} />);
    expect(screen.getByRole("button").textContent).toContain("explore");
    expect(screen.getByRole("button").textContent).toContain("2 steps");
  });

  it("retires the live panel once the result arrives", () => {
    const tc = spawnCall({
      state: "done",
      subagent: activity({ phase: "finished" }),
      resultText: JSON.stringify({
        result: "Three emails: …",
        success: true,
        role: "explore",
        child_session_id: "subagent-abc12345-0000-0000-0000-000000000000",
        cost_usd: 0.0123,
        tokens: 4210,
        steps: 3,
        tools_used: ["outlook_email_search", "view_file"],
      }),
    });
    render(<ToolChip tc={tc} taskTrackerDisplay={null} />);
    expect(screen.queryByTestId("chat-subagent-activity")).toBeNull();
    const card = screen.getByTestId("chat-subagent-card");
    expect(card.textContent).toContain("done");
    expect(card.textContent).toContain("3 steps");
    expect(card.textContent).toContain("outlook_email_search, view_file");
    expect(card.textContent).toContain("$0.0123");
  });

  it("keeps a failed child legible: status, spend and the steps it did take", () => {
    const tc = spawnCall({
      state: "done",
      resultText: JSON.stringify({
        result: "[sub-agent produced no final answer]",
        success: false,
        role: "worker",
        child_session_id: "subagent-abc12345-0000-0000-0000-000000000000",
        cost_usd: 0.004,
        tokens: 900,
        steps: 5,
        tools_used: ["bash"],
      }),
    });
    render(<ToolChip tc={tc} taskTrackerDisplay={null} />);
    const card = screen.getByTestId("chat-subagent-card");
    expect(card.textContent).toContain("failed");
    expect(card.textContent).toContain("5 steps");
    expect(card.textContent).toContain("bash");
  });
});
