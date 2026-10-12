import { describe, expect, it } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import { ToolChip, isStagedForApproval, stagedApprovalIds } from "./ToolChips";
import type { ToolCall } from "./history";

// A critical call fleet stages for approval resolves its tool call with the
// gate's is_err APPROVAL_REQUIRED placeholder, so the chip used to render red,
// as if the call had failed, right above the card asking the person to
// approve it. A staged call is waiting, not failed.

const APPROVAL_ID = "4f1d2c3b-aaaa-bbbb-cccc-000000000001";
const STAGED =
  `APPROVAL_REQUIRED: mcp_indexexchange_mcp_ix_execute_plan is a critical action and has been staged for explicit user approval (approval_id=${APPROVAL_ID}). ` +
  "Do NOT retry, and do NOT attempt the same change through a different tool.";

function call(resultText: string, state: ToolCall["state"] = "error"): ToolCall {
  return { id: "call_1", name: "mcp_indexexchange_mcp_ix_execute_plan", input: "{}", resultText, state };
}

describe("isStagedForApproval", () => {
  const ids = stagedApprovalIds([{ id: APPROVAL_ID }]);

  it("is true for the gate's placeholder naming a card of the message", () => {
    expect(isStagedForApproval(call(STAGED), ids)).toBe(true);
  });

  it("covers every gate wording that names an approval id", () => {
    for (const text of [
      `APPROVAL_REQUIRED: this send_email call has been staged for explicit user approval (approval_id=${APPROVAL_ID}). Do NOT retry.`,
      `APPROVAL_REQUIRED: risky command — staged for user approval (approval_id=${APPROVAL_ID}). Do NOT retry.`,
      `APPROVAL_REQUIRED: the scheduled task has been staged for explicit user approval (approval_id=${APPROVAL_ID}). Do NOT retry.`,
    ]) {
      expect(isStagedForApproval(call(text), ids)).toBe(true);
    }
  });

  it("stays an error when staging failed (no approval id)", () => {
    const failed =
      "APPROVAL_REQUIRED: mcp_x_update_deal is a critical action. Could not stage it for user approval (db down). Ask the user what to do.";
    expect(isStagedForApproval(call(failed), ids)).toBe(false);
  });

  it("stays an error when the id names no card of the message", () => {
    expect(isStagedForApproval(call(STAGED), stagedApprovalIds([{ id: "another-card" }]))).toBe(false);
    expect(isStagedForApproval(call(STAGED), stagedApprovalIds(undefined))).toBe(false);
  });

  it("stays an error for a tool's own text that only contains the words", () => {
    expect(isStagedForApproval(call(`upstream said: ${STAGED}`), ids)).toBe(false);
    expect(isStagedForApproval(call("APPROVAL_BLOCKED: another write is waiting"), ids)).toBe(false);
  });

  it("only applies to an error chip", () => {
    expect(isStagedForApproval(call(STAGED, "done"), ids)).toBe(false);
    expect(isStagedForApproval(call(STAGED, "pending"), ids)).toBe(false);
  });
});

describe("ToolChip for a staged call", () => {
  it("renders neutral, marked for approval, and its result is not shown as an error", () => {
    render(<ToolChip tc={call(STAGED)} taskTrackerDisplay={null} stagedForApproval />);
    const chip = screen.getByRole("button");
    expect(chip.getAttribute("style")).not.toContain("--color-danger");
    expect(screen.getByTestId("tool-chip-staged").textContent).toContain("for approval");
    fireEvent.click(chip);
    expect(screen.getByText(/staged for explicit user approval/)).toBeTruthy();
  });

  it("keeps the red error chip by default", () => {
    render(<ToolChip tc={call(STAGED)} taskTrackerDisplay={null} />);
    const chip = screen.getByRole("button");
    expect(chip.getAttribute("style")).toContain("--color-danger");
    expect(screen.queryByTestId("tool-chip-staged")).toBeNull();
  });
});
