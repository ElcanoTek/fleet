import { describe, expect, it, vi } from "vitest";
import { useRef, useState } from "react";
import { fireEvent, render, screen } from "@testing-library/react";
import { DialogShell } from "./DialogShell";

// DialogShell is the one modal-dialog base for the chat and settings surfaces
// (QA finding B-2, widened to every modal). The defect it fixes is a
// *transparent* panel — half the dialogs painted themselves with
// color-mix() over the --composer-surface GRADIENT, which never resolves, so
// the background declaration was dropped and the page read straight through
// the dialog. These assert the contract every consumer inherits: an opaque
// panel, a scrim that cancels, Escape, focus, and one-layer-at-a-time
// dismissal when dialogs stack.

describe("DialogShell", () => {
  it("paints an opaque panel over the app's own scrim", () => {
    render(
      <DialogShell label="A dialog" scrimLabel="Close it" onDismiss={() => {}}>
        <p>Body</p>
      </DialogShell>,
    );
    const panel = screen.getByRole("dialog", { name: "A dialog" });
    expect(panel).toHaveAttribute("aria-modal", "true");
    // Opaque surface token, not a translucent composer panel.
    expect(panel.className).toContain("bg-[var(--color-surface-1)]");
    expect(panel.className).toContain("shadow-[var(--shadow-md)]");
    expect(panel.className).not.toContain("composer-surface");
    expect(panel.className).not.toContain("backdrop-blur");
    expect(screen.getByLabelText("Close it").className).toContain(
      "bg-[var(--color-overlay-strong)]",
    );
  });

  it("takes its sizing from the consumer and its surface from itself", () => {
    render(
      <DialogShell
        label="A dialog"
        scrimLabel="Close it"
        onDismiss={() => {}}
        className="max-w-[26rem] p-5"
        testId="panel"
      >
        <p>Body</p>
      </DialogShell>,
    );
    const panel = screen.getByTestId("panel");
    expect(panel.className).toContain("max-w-[26rem]");
    expect(panel.className).toContain("p-5");
    expect(panel.className).toContain("bg-[var(--color-surface-1)]");
  });

  it("dismisses from the scrim and from Escape", () => {
    const onDismiss = vi.fn();
    render(
      <DialogShell label="A dialog" scrimLabel="Close it" onDismiss={onDismiss}>
        <p>Body</p>
      </DialogShell>,
    );
    fireEvent.click(screen.getByLabelText("Close it"));
    expect(onDismiss).toHaveBeenCalledTimes(1);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onDismiss).toHaveBeenCalledTimes(2);
  });

  it("names itself from its own copy when there is no title", () => {
    render(
      <DialogShell
        labelledBy="the-body"
        scrimLabel="Close it"
        onDismiss={() => {}}
      >
        <p id="the-body">Really do the thing?</p>
      </DialogShell>,
    );
    expect(
      screen.getByRole("dialog", { name: "Really do the thing?" }),
    ).toHaveAttribute("aria-labelledby", "the-body");
  });

  it("moves focus into the panel, and to the initial-focus target when given", () => {
    const { unmount } = render(
      <DialogShell label="A dialog" scrimLabel="Close it" onDismiss={() => {}}>
        <p>Body</p>
      </DialogShell>,
    );
    expect(document.activeElement).toBe(screen.getByRole("dialog"));
    unmount();

    function WithInput() {
      const inputRef = useRef<HTMLInputElement>(null);
      return (
        <DialogShell
          label="Filtered"
          scrimLabel="Close it"
          onDismiss={() => {}}
          initialFocusRef={inputRef}
        >
          <input ref={inputRef} aria-label="Filter" />
        </DialogShell>
      );
    }
    render(<WithInput />);
    expect(document.activeElement).toBe(screen.getByLabelText("Filter"));
  });

  it("hands focus back to whatever opened it", () => {
    function Host() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <button type="button" onClick={() => setOpen(true)}>
            Open
          </button>
          {open ? (
            <DialogShell
              label="A dialog"
              scrimLabel="Close it"
              onDismiss={() => setOpen(false)}
            >
              <p>Body</p>
            </DialogShell>
          ) : null}
        </>
      );
    }
    render(<Host />);
    const trigger = screen.getByRole("button", { name: "Open" });
    trigger.focus();
    fireEvent.click(trigger);
    expect(document.activeElement).toBe(screen.getByRole("dialog"));
    fireEvent.keyDown(document, { key: "Escape" });
    expect(document.activeElement).toBe(trigger);
  });

  it("stacks: Escape dismisses only the dialog on top", () => {
    const onOuter = vi.fn();
    const onInner = vi.fn();
    const { rerender } = render(
      <DialogShell label="Outer" scrimLabel="Close outer" onDismiss={onOuter}>
        <p>Outer body</p>
      </DialogShell>,
    );
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onOuter).toHaveBeenCalledTimes(1);

    // The inner dialog is summoned from the outer one, so it mounts second.
    rerender(
      <>
        <DialogShell label="Outer" scrimLabel="Close outer" onDismiss={onOuter}>
          <p>Outer body</p>
        </DialogShell>
        <DialogShell
          label="Inner"
          scrimLabel="Close inner"
          onDismiss={onInner}
          layer="stacked"
        >
          <p>Inner body</p>
        </DialogShell>
      </>,
    );
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onInner).toHaveBeenCalledTimes(1);
    expect(onOuter).toHaveBeenCalledTimes(1);
  });

  it("paints a stacked dialog above ordinary modal peers", () => {
    const { rerender } = render(
      <DialogShell label="Plain" scrimLabel="Close it" onDismiss={() => {}}>
        <p>Body</p>
      </DialogShell>,
    );
    expect(screen.getByRole("dialog").parentElement?.className).toContain(
      "z-50",
    );
    rerender(
      <DialogShell
        label="Plain"
        scrimLabel="Close it"
        onDismiss={() => {}}
        layer="stacked"
      >
        <p>Body</p>
      </DialogShell>,
    );
    expect(screen.getByRole("dialog").parentElement?.className).toContain(
      "z-[60]",
    );
  });
});
