// ConfirmDialog is summoned from INSIDE other modals (Stop/Delete in the task
// modal footer). Peer .modal-overlay layers tie on z-index and resolve by DOM
// order, and the task modal renders later — so without its own stacking class
// the dialog painted BEHIND the modal that asked the question (field report,
// 2026-08-19: "delete confirmation appears behind the window"). The class is
// the fix; this pins it plus the basic confirm/cancel contract.

import { describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach } from "vitest";
import { ConfirmDialog } from "./ConfirmDialog";

afterEach(() => cleanup());

describe("ConfirmDialog", () => {
  it("stacks above ordinary modal overlays via confirm-overlay", () => {
    render(
      <ConfirmDialog open message="Delete it?" onConfirm={() => {}} onCancel={() => {}} />,
    );
    const overlay = screen.getByRole("dialog");
    expect(overlay.className).toContain("modal-overlay");
    expect(overlay.className).toContain("confirm-overlay");
  });

  it("fires onConfirm / onCancel and renders nothing when closed", () => {
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    const { rerender } = render(
      <ConfirmDialog
        open
        message="Delete it?"
        confirmLabel="Delete"
        onConfirm={onConfirm}
        onCancel={onCancel}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onCancel).toHaveBeenCalledTimes(1);

    rerender(
      <ConfirmDialog
        open={false}
        message="Delete it?"
        onConfirm={onConfirm}
        onCancel={onCancel}
      />,
    );
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
