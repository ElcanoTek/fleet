// ConfirmDialog is summoned from INSIDE other modals (Stop/Delete in the task
// modal footer). Peer .modal-overlay layers tie on z-index and resolve by DOM
// order, and the task modal renders later — so without its own stacking class
// the dialog painted BEHIND the modal that asked the question (field report,
// 2026-08-19: "delete confirmation appears behind the window"). The class is
// the fix; this pins it plus the basic confirm/cancel contract — and the
// keyboard contract (Escape, focus in/trap/restore, busy) it gained later.

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

  it("moves focus to Cancel on open and returns it to the opener on close", () => {
    const opener = document.createElement("button");
    opener.textContent = "Delete task";
    document.body.appendChild(opener);
    opener.focus();
    try {
      const { rerender } = render(
        <ConfirmDialog open={false} message="Delete it?" onConfirm={() => {}} onCancel={() => {}} />,
      );
      rerender(
        <ConfirmDialog open message="Delete it?" onConfirm={() => {}} onCancel={() => {}} />,
      );
      // The safe action, not the destructive one, is what Enter would hit.
      expect(screen.getByRole("button", { name: "Cancel" })).toHaveFocus();
      rerender(
        <ConfirmDialog open={false} message="Delete it?" onConfirm={() => {}} onCancel={() => {}} />,
      );
      expect(opener).toHaveFocus();
    } finally {
      opener.remove();
    }
  });

  it("Escape cancels a confirm and acknowledges an alert", () => {
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    const { unmount } = render(
      <ConfirmDialog open message="Delete it?" onConfirm={onConfirm} onCancel={onCancel} />,
    );
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onCancel).toHaveBeenCalledTimes(1);
    expect(onConfirm).not.toHaveBeenCalled();
    unmount();

    const onOk = vi.fn();
    render(<ConfirmDialog open message="Saved." onConfirm={onOk} />);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onOk).toHaveBeenCalledTimes(1);
  });

  it("busy disables confirm and ignores Escape/Cancel", () => {
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    render(
      <ConfirmDialog
        open
        busy
        message="Delete it?"
        confirmLabel="Deleting…"
        onConfirm={onConfirm}
        onCancel={onCancel}
      />,
    );
    const confirm = screen.getByRole("button", { name: "Deleting…" });
    expect(confirm).toBeDisabled();
    fireEvent.click(confirm);
    expect(onConfirm).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onCancel).not.toHaveBeenCalled();
    expect(screen.getByRole("dialog")).toHaveAttribute("aria-busy", "true");
  });
});
