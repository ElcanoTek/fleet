import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { ChatReorderList } from "./ChatReorderList";

// The rail's reorder mode for pinned or project chats (docs/CHAT-ORDER.md): arrows
// move a chat one place and announce it, the ends cannot move further, a
// pointer drag (a mouse on the row, a finger on the handle) drops a chat where
// it lands, and Done or Escape leaves the mode.

afterEach(cleanup);

const chats = [
  { id: "a", title: "Alpha" },
  { id: "b", title: "Bravo" },
  { id: "c", title: "Charlie" },
];

describe("ChatReorderList", () => {
  it("names the list and says what the order is for", () => {
    render(<ChatReorderList label="Chats in Roadmap, in order" hint="Set the order." chats={chats} onReorder={() => {}} onDone={() => {}} />);
    expect(screen.getByRole("list", { name: "Chats in Roadmap, in order" })).toBeTruthy();
    expect(screen.getByText("Set the order.")).toBeTruthy();
  });

  it("moves a chat with its arrows and announces the move", () => {
    const onReorder = vi.fn();
    render(<ChatReorderList label="Pinned chats, in order" hint="Set the order." chats={chats} onReorder={onReorder} onDone={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: "Move Charlie up" }));
    expect(onReorder).toHaveBeenLastCalledWith(["a", "c", "b"]);
    expect(screen.getByRole("status").textContent).toBe("Charlie moved to position 2 of 3.");
    fireEvent.click(screen.getByRole("button", { name: "Move Alpha down" }));
    expect(onReorder).toHaveBeenLastCalledWith(["b", "a", "c"]);
  });

  it("disables moving past either end", () => {
    render(<ChatReorderList label="Pinned chats, in order" hint="Set the order." chats={chats} onReorder={() => {}} onDone={() => {}} />);
    expect((screen.getByRole("button", { name: "Move Alpha up" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Move Charlie down" }) as HTMLButtonElement).disabled).toBe(true);
  });

  // jsdom lays nothing out: give the rows the 40px pitch a real list has.
  const layOut = () => {
    chats.forEach((c, i) => {
      const li = screen.getByTestId(`chat-reorder-item-${c.id}`);
      li.getBoundingClientRect = () => ({ top: i * 40, bottom: i * 40 + 36, left: 0, right: 200, width: 200, height: 36, x: 0, y: i * 40, toJSON: () => ({}) });
    });
  };
  const rowOf = (id: string) => screen.getByTestId(`chat-reorder-item-${id}`).firstElementChild as HTMLElement;
  const pointer = (type: string, y: number) => ({ pointerId: 1, pointerType: type, button: 0, clientY: y });

  it("drops a chat dragged with a mouse where it lands, and shows the gap on the way", () => {
    const onReorder = vi.fn();
    render(<ChatReorderList label="Pinned chats, in order" hint="Set the order." chats={chats} onReorder={onReorder} onDone={() => {}} />);
    layOut();
    fireEvent.pointerDown(rowOf("c"), pointer("mouse", 100));
    fireEvent.pointerMove(rowOf("c"), pointer("mouse", 100 - 75));
    // Carried two places up: Charlie follows the pointer, the two it passed step down.
    expect(screen.getByTestId("chat-reorder-item-c").style.transform).toBe("translateY(-75px)");
    expect(screen.getByTestId("chat-reorder-item-a").style.transform).toBe("translateY(40px)");
    expect(screen.getByTestId("chat-reorder-item-b").style.transform).toBe("translateY(40px)");
    expect(onReorder).not.toHaveBeenCalled();
    fireEvent.pointerUp(rowOf("c"), pointer("mouse", 100 - 75));
    expect(onReorder).toHaveBeenCalledTimes(1);
    expect(onReorder).toHaveBeenCalledWith(["c", "a", "b"]);
    expect(screen.getByRole("status").textContent).toBe("Charlie moved to position 1 of 3.");
    expect(screen.getByTestId("chat-reorder-item-c").style.transform).toBe("");
  });

  it("keeps a row carried past the end of the list in the last place", () => {
    const onReorder = vi.fn();
    render(<ChatReorderList label="Pinned chats, in order" hint="Set the order." chats={chats} onReorder={onReorder} onDone={() => {}} />);
    layOut();
    fireEvent.pointerDown(rowOf("a"), pointer("mouse", 20));
    fireEvent.pointerMove(rowOf("a"), pointer("mouse", 2000));
    // Two places down is as far as it goes, however far the pointer went.
    expect(screen.getByTestId("chat-reorder-item-a").style.transform).toBe("translateY(80px)");
    fireEvent.pointerUp(rowOf("a"), pointer("mouse", 2000));
    expect(onReorder).toHaveBeenCalledWith(["b", "c", "a"]);
  });

  it("drags with a finger by the handle only, so touching a row still scrolls the list", () => {
    const onReorder = vi.fn();
    render(<ChatReorderList label="Pinned chats, in order" hint="Set the order." chats={chats} onReorder={onReorder} onDone={() => {}} />);
    layOut();
    // A finger on the row itself starts nothing.
    fireEvent.pointerDown(rowOf("a"), pointer("touch", 20));
    fireEvent.pointerMove(rowOf("a"), pointer("touch", 100));
    fireEvent.pointerUp(rowOf("a"), pointer("touch", 100));
    expect(onReorder).not.toHaveBeenCalled();
    // On the handle it drags; the handle does not scroll under the finger.
    const handle = screen.getByTestId("chat-reorder-handle-a");
    expect(handle.className).toContain("touch-none");
    fireEvent.pointerDown(handle, pointer("touch", 20));
    fireEvent.pointerMove(handle, pointer("touch", 20 + 44));
    fireEvent.pointerUp(handle, pointer("touch", 20 + 44));
    expect(onReorder).toHaveBeenCalledWith(["b", "a", "c"]);
  });

  it("puts the row back when a drag is cancelled, and Escape cancels the drag before it leaves the mode", () => {
    const onReorder = vi.fn();
    const onDone = vi.fn();
    render(<ChatReorderList label="Pinned chats, in order" hint="Set the order." chats={chats} onReorder={onReorder} onDone={onDone} />);
    layOut();
    fireEvent.pointerDown(rowOf("a"), pointer("mouse", 20));
    fireEvent.pointerMove(rowOf("a"), pointer("mouse", 100));
    fireEvent.pointerCancel(rowOf("a"), pointer("mouse", 100));
    expect(onReorder).not.toHaveBeenCalled();
    expect(screen.getByTestId("chat-reorder-item-a").style.transform).toBe("");

    fireEvent.pointerDown(rowOf("a"), pointer("mouse", 20));
    fireEvent.pointerMove(rowOf("a"), pointer("mouse", 100));
    fireEvent.keyDown(window, { key: "Escape" });
    expect(onDone).not.toHaveBeenCalled();
    fireEvent.pointerUp(rowOf("a"), pointer("mouse", 100));
    expect(onReorder).not.toHaveBeenCalled();
    fireEvent.keyDown(window, { key: "Escape" });
    expect(onDone).toHaveBeenCalledTimes(1);
  });

  it("does not start a drag from an arrow button, or drop a chat where it started", () => {
    const onReorder = vi.fn();
    render(<ChatReorderList label="Pinned chats, in order" hint="Set the order." chats={chats} onReorder={onReorder} onDone={() => {}} />);
    layOut();
    const up = screen.getByRole("button", { name: "Move Bravo up" });
    fireEvent.pointerDown(up, pointer("mouse", 60));
    fireEvent.pointerMove(rowOf("b"), pointer("mouse", 0));
    fireEvent.pointerUp(rowOf("b"), pointer("mouse", 0));
    expect(onReorder).not.toHaveBeenCalled();
    // A press and release with almost no travel is not a move.
    fireEvent.pointerDown(rowOf("b"), pointer("mouse", 60));
    fireEvent.pointerMove(rowOf("b"), pointer("mouse", 66));
    fireEvent.pointerUp(rowOf("b"), pointer("mouse", 66));
    expect(onReorder).not.toHaveBeenCalled();
  });

  it("leaves Done to the caller's heading when asked", () => {
    render(<ChatReorderList label="Pinned chats, in order" hint="Set the order." chats={chats} onReorder={() => {}} onDone={() => {}} doneInHeading />);
    expect(screen.queryByTestId("chat-reorder-done")).toBeNull();
  });

  it("takes Escape for itself, so the rail's own Escape does not also fire", () => {
    const onDone = vi.fn();
    const railEscape = vi.fn();
    window.addEventListener("keydown", railEscape);
    try {
      render(<ChatReorderList label="Pinned chats, in order" hint="Set the order." chats={chats} onReorder={() => {}} onDone={onDone} />);
      fireEvent.keyDown(document.body, { key: "Escape" });
      expect(onDone).toHaveBeenCalledTimes(1);
      expect(railEscape).not.toHaveBeenCalled();
    } finally {
      window.removeEventListener("keydown", railEscape);
    }
  });

  it("puts the focus on the list on entry, and keeps it on a live arrow after a move", async () => {
    const { rerender } = render(<ChatReorderList label="Pinned chats, in order" hint="Set the order." chats={chats} onReorder={() => {}} onDone={() => {}} />);
    // Alpha's up arrow is disabled, so its down arrow is the first control.
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Move Alpha down" }));
    // Bravo up: it becomes first, its up arrow disabled, so focus goes to its down arrow.
    fireEvent.click(screen.getByRole("button", { name: "Move Bravo up" }));
    rerender(
      <ChatReorderList label="Pinned chats, in order" hint="Set the order." chats={[chats[1], chats[0], chats[2]]} onReorder={() => {}} onDone={() => {}} />,
    );
    await new Promise((r) => requestAnimationFrame(() => r(null)));
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Move Bravo down" }));
  });

  it("leaves the mode on Done and on Escape", () => {
    const onDone = vi.fn();
    render(<ChatReorderList label="Pinned chats, in order" hint="Set the order." chats={chats} onReorder={() => {}} onDone={onDone} />);
    fireEvent.click(screen.getByTestId("chat-reorder-done"));
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onDone).toHaveBeenCalledTimes(2);
  });
});
