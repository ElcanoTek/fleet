import { render } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { DashboardBoard } from "./DashboardView";
import type { BoardPanel } from "./board";

const panel = (key: string, aliases: string[] = []): BoardPanel =>
  ({
    key,
    aliases,
    cardId: "c1",
    call: { id: "c1", name: "show_ui", input: "{}", state: "done" },
    spec: { title: "Revenue", components: [{ type: "text", text: "x" }] },
    version: 1,
    occurrence: 0,
    messageIndex: 0,
    interactive: false,
    wide: false,
    pinned: true,
  }) as BoardPanel;

const props = {
  summaryIndex: -1,
  liveMessageIndex: -1,
  storageScope: "conv",
  submissions: new Map(),
  replies: new Map(),
  onSubmit: vi.fn(),
  updatePrefs: vi.fn(),
  onStarter: vi.fn(),
};

describe("DashboardBoard focus layer", () => {
  it("follows the focused panel when a refresh moves its slot key", () => {
    const setMaximized = vi.fn();
    const { rerender } = render(<DashboardBoard {...props} panels={[panel("c:c1")]} maximized="c:c1" setMaximized={setMaximized} />);
    expect(setMaximized).not.toHaveBeenCalled();
    rerender(<DashboardBoard {...props} panels={[panel("p:rev", ["c:c1"])]} maximized="c:c1" setMaximized={setMaximized} />);
    expect(setMaximized).toHaveBeenCalledWith("p:rev");
  });

  it("closes, with the focus back on the board, when its panel leaves the board", () => {
    const setMaximized = vi.fn();
    const { getByTestId } = render(<DashboardBoard {...props} panels={[]} maximized="c:gone" setMaximized={setMaximized} />);
    expect(setMaximized).toHaveBeenCalledWith("");
    expect(document.activeElement).toBe(getByTestId("dashboard-board"));
  });
});
