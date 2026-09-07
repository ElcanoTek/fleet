// The hook's one cross-dialog rule: when a confirm is summoned from inside
// another modal, only the topmost dialog reacts to Escape and Tab. Before the
// dialog stack, both document-level capture listeners fired — Escape in the
// delete confirmation also closed the log viewer under it — because
// preventDefault does not stop propagation to a sibling listener.

import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useRef } from "react";
import { useDialogA11y } from "./useDialogA11y";

afterEach(() => cleanup());

function Dialog({
  name,
  onClose,
  children,
}: {
  name: string;
  onClose: () => void;
  children?: React.ReactNode;
}) {
  const ref = useRef<HTMLDivElement | null>(null);
  useDialogA11y(true, ref, onClose);
  return (
    <div role="dialog" aria-label={name} ref={ref} tabIndex={-1}>
      <button type="button">{name} first</button>
      <button type="button">{name} last</button>
      {children}
    </div>
  );
}

describe("useDialogA11y stacking", () => {
  it("routes Escape to the topmost dialog only, then to the one beneath once it closes", () => {
    const closeOuter = vi.fn();
    const closeInner = vi.fn();
    const { rerender } = render(
      <>
        <Dialog name="outer" onClose={closeOuter} />
        <Dialog name="inner" onClose={closeInner} />
      </>,
    );

    fireEvent.keyDown(document.body, { key: "Escape" });
    expect(closeInner).toHaveBeenCalledTimes(1);
    expect(closeOuter).not.toHaveBeenCalled();

    // The inner dialog goes away; the outer one is topmost again.
    rerender(<Dialog name="outer" onClose={closeOuter} />);
    fireEvent.keyDown(document.body, { key: "Escape" });
    expect(closeOuter).toHaveBeenCalledTimes(1);
    expect(closeInner).toHaveBeenCalledTimes(1);
  });

  it("lets only the topmost dialog trap Tab", () => {
    render(
      <>
        <Dialog name="outer" onClose={() => {}} />
        <Dialog name="inner" onClose={() => {}} />
      </>,
    );
    const innerLast = screen.getByRole("button", { name: "inner last" });
    innerLast.focus();
    // The outer trap sees the active element outside ITS container and, if it
    // were still listening, would yank focus to the outer dialog. With the
    // stack it yields, so Tab keeps focus inside the inner dialog. (jsdom has
    // no layout — offsetParent is always null — so the inner trap's focusable
    // set is just the active element and Tab wraps onto itself; the assertion
    // is therefore "still inside inner", not a specific control.)
    fireEvent.keyDown(document.body, { key: "Tab" });
    expect(screen.getByRole("dialog", { name: "inner" }).contains(document.activeElement)).toBe(true);
    expect(screen.getByRole("dialog", { name: "outer" })).not.toBe(document.activeElement);
  });
});
