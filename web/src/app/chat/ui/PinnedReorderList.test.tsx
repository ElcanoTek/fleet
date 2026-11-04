import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, createEvent, fireEvent, render, screen } from "@testing-library/react";
import { PinnedReorderList } from "./PinnedReorderList";

// The rail's reorder mode for pinned chats (docs/CHAT-PIN-ORDER.md): arrows
// move a chat one place and announce it, the ends cannot move further, a drag
// drops a chat where it lands, and Done or Escape leaves the mode.

afterEach(cleanup);

const chats = [
  { id: "a", title: "Alpha" },
  { id: "b", title: "Bravo" },
  { id: "c", title: "Charlie" },
];

describe("PinnedReorderList", () => {
  it("moves a chat with its arrows and announces the move", () => {
    const onReorder = vi.fn();
    render(<PinnedReorderList chats={chats} onReorder={onReorder} onDone={() => {}} />);
    fireEvent.click(screen.getByRole("button", { name: "Move Charlie up" }));
    expect(onReorder).toHaveBeenLastCalledWith(["a", "c", "b"]);
    expect(screen.getByRole("status").textContent).toBe("Charlie moved to position 2 of 3.");
    fireEvent.click(screen.getByRole("button", { name: "Move Alpha down" }));
    expect(onReorder).toHaveBeenLastCalledWith(["b", "a", "c"]);
  });

  it("disables moving past either end", () => {
    render(<PinnedReorderList chats={chats} onReorder={() => {}} onDone={() => {}} />);
    expect((screen.getByRole("button", { name: "Move Alpha up" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Move Charlie down" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("drops a dragged chat where it lands", () => {
    const onReorder = vi.fn();
    render(<PinnedReorderList chats={chats} onReorder={onReorder} onDone={() => {}} />);
    const store: Record<string, string> = {};
    const dataTransfer = {
      setData: (k: string, v: string) => (store[k] = v),
      getData: (k: string) => store[k] ?? "",
      get types() {
        return Object.keys(store);
      },
      effectAllowed: "",
      dropEffect: "",
    };
    const row = (id: string) => screen.getByTestId(`pinned-reorder-item-${id}`).firstElementChild as HTMLElement;
    fireEvent.dragStart(row("c"), { dataTransfer });
    const over = createEvent.dragOver(row("a"), { dataTransfer });
    fireEvent(row("a"), over);
    expect(over.defaultPrevented).toBe(true);
    fireEvent.drop(row("a"), { dataTransfer });
    expect(onReorder).toHaveBeenCalledWith(["c", "a", "b"]);
  });

  it("ignores a drag that is not a reorder (a chat dragged toward a project)", () => {
    render(<PinnedReorderList chats={chats} onReorder={() => {}} onDone={() => {}} />);
    const row = screen.getByTestId("pinned-reorder-item-a").firstElementChild as HTMLElement;
    const over = createEvent.dragOver(row, { dataTransfer: { types: ["application/x-fleet-conversation"] } });
    fireEvent(row, over);
    expect(over.defaultPrevented).toBe(false);
  });

  it("leaves the mode on Done and on Escape", () => {
    const onDone = vi.fn();
    render(<PinnedReorderList chats={chats} onReorder={() => {}} onDone={onDone} />);
    fireEvent.click(screen.getByTestId("pinned-reorder-done"));
    fireEvent.keyDown(document, { key: "Escape" });
    expect(onDone).toHaveBeenCalledTimes(2);
  });
});
