import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, createEvent, fireEvent, render, screen } from "@testing-library/react";
import { ChatReorderList } from "./ChatReorderList";

// The rail's reorder mode for pinned or project chats (docs/CHAT-ORDER.md): arrows
// move a chat one place and announce it, the ends cannot move further, a drag
// drops a chat where it lands, and Done or Escape leaves the mode.

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

  it("drops a dragged chat where it lands", () => {
    const onReorder = vi.fn();
    render(<ChatReorderList label="Pinned chats, in order" hint="Set the order." chats={chats} onReorder={onReorder} onDone={() => {}} />);
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
    const row = (id: string) => screen.getByTestId(`chat-reorder-item-${id}`).firstElementChild as HTMLElement;
    fireEvent.dragStart(row("c"), { dataTransfer });
    const over = createEvent.dragOver(row("a"), { dataTransfer });
    fireEvent(row("a"), over);
    expect(over.defaultPrevented).toBe(true);
    fireEvent.drop(row("a"), { dataTransfer });
    expect(onReorder).toHaveBeenCalledWith(["c", "a", "b"]);
  });

  it("ignores a drag that is not a reorder (a chat dragged toward a project)", () => {
    render(<ChatReorderList label="Pinned chats, in order" hint="Set the order." chats={chats} onReorder={() => {}} onDone={() => {}} />);
    const row = screen.getByTestId("chat-reorder-item-a").firstElementChild as HTMLElement;
    const over = createEvent.dragOver(row, { dataTransfer: { types: ["application/x-fleet-conversation"] } });
    fireEvent(row, over);
    expect(over.defaultPrevented).toBe(false);
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
