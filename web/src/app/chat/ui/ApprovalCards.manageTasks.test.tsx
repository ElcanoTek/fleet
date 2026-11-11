import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { ApprovalCard } from "./ApprovalCards";
import type { Approval } from "./history";

// A user asked for a small change to a scheduled email and approved a card
// that showed only the first 80 characters of a full prompt replacement; the
// rules the old prompt carried were silently dropped. The card now shows the
// prompt text itself, and says which kind of change it is.

// The manage_tasks fields are not in the shared summary type; the card reads
// them through its own cast, so the test passes them the same way.
function renderManageTasks(summary: Record<string, unknown>) {
  const approval: Approval = {
    id: "ap_manage",
    tool: "manage_tasks",
    status: "pending",
    summary: { tool: "manage_tasks", action: "update", task_ids: ["t1"], match: [], changes: [], max_tasks: 25, ...summary },
  } as Approval;
  render(<ApprovalCard approval={approval} conversationId="conv_1" onResolved={() => {}} />);
}

describe("the manage_tasks card's prompt changes", () => {
  it("shows each exact-text edit as the text it replaces and the text it becomes", () => {
    renderManageTasks({
      prompt_edits: [
        { old: "last 7 days", new: "last 14 days" },
        { old: "Send it.\nTwice.", new: "" },
      ],
    });
    const block = screen.getByTestId("manage-tasks-prompt-edits");
    expect(block.textContent).toContain("everything else is kept");
    expect(block.querySelectorAll("del")[0]?.textContent).toBe("last 7 days");
    expect(block.querySelectorAll("ins")[0]?.textContent).toBe("last 14 days");
    expect(block.querySelectorAll("del")[1]?.textContent).toBe("Send it.\nTwice.");
    expect(block.textContent).toContain("(removed)");
    expect(screen.queryByTestId("manage-tasks-prompt-replacement")).toBeNull();
  });

  it("shows appended text", () => {
    renderManageTasks({ prompt_append: "Add a 14-day trend section." });
    expect(screen.getByTestId("manage-tasks-prompt-append").textContent).toContain("Add a 14-day trend section.");
  });

  it("shows a full replacement whole, under a warning that it replaces everything", () => {
    renderManageTasks({ prompt_replacement: "A brand new prompt.\nSecond line." });
    const block = screen.getByTestId("manage-tasks-prompt-replacement");
    expect(block.textContent).toContain("replaces each task's whole current prompt");
    expect(block.textContent).toContain("A brand new prompt.\nSecond line.");
  });

  it("renders no prompt blocks for a change that does not touch the prompt", () => {
    renderManageTasks({ changes: ["schedule → 0 7 * * *"] });
    expect(screen.queryByTestId("manage-tasks-prompt-edits")).toBeNull();
    expect(screen.queryByTestId("manage-tasks-prompt-append")).toBeNull();
    expect(screen.queryByTestId("manage-tasks-prompt-replacement")).toBeNull();
  });
});
