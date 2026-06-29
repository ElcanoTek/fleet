import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useRef, useState } from "react";
import { Menu, MenuItem } from "./Menu";

afterEach(() => cleanup());

function Harness({ onClose }: { onClose?: () => void }) {
  const [open, setOpen] = useState(false);
  const anchorRef = useRef<HTMLButtonElement | null>(null);
  const close = () => {
    setOpen(false);
    onClose?.();
  };
  return (
    <>
      <button ref={anchorRef} type="button" onClick={() => setOpen(true)}>
        Open
      </button>
      <Menu open={open} onClose={close} anchorRef={anchorRef} label="Test">
        <MenuItem onClick={() => {}}>First</MenuItem>
        <MenuItem onClick={() => {}}>Second</MenuItem>
        <MenuItem onClick={() => {}}>Third</MenuItem>
      </Menu>
    </>
  );
}

describe("Menu", () => {
  it("focuses the first item on open", () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole("button", { name: "Open" }));
    expect(document.activeElement).toBe(screen.getByRole("menuitem", { name: "First" }));
  });

  it("moves focus with ArrowDown/ArrowUp and wraps", () => {
    render(<Harness />);
    fireEvent.click(screen.getByRole("button", { name: "Open" }));
    const menu = screen.getByRole("menu", { name: "Test" });
    fireEvent.keyDown(menu, { key: "ArrowDown" });
    expect(document.activeElement).toBe(screen.getByRole("menuitem", { name: "Second" }));
    fireEvent.keyDown(menu, { key: "ArrowUp" });
    expect(document.activeElement).toBe(screen.getByRole("menuitem", { name: "First" }));
    // Wrap past the top to the last item.
    fireEvent.keyDown(menu, { key: "ArrowUp" });
    expect(document.activeElement).toBe(screen.getByRole("menuitem", { name: "Third" }));
  });

  it("closes on Escape and returns focus to the anchor", () => {
    const onClose = vi.fn();
    render(<Harness onClose={onClose} />);
    const anchor = screen.getByRole("button", { name: "Open" });
    fireEvent.click(anchor);
    const menu = screen.getByRole("menu", { name: "Test" });
    fireEvent.keyDown(menu, { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("menu", { name: "Test" })).toBeNull();
    expect(document.activeElement).toBe(anchor);
  });
});
