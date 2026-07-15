import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { MessageMinimap, type MinimapEntry } from "./MessageMinimap";

// The Codex-style jump rail: a dash per recent user message. Click jumps,
// press-drag scrubs (pointer Y → entry), hover shows a preview card with the
// user text + the assistant reply snippet.

afterEach(() => cleanup());

const entries: MinimapEntry[] = [
  { id: 1, userText: "first question", replySnippet: "first answer …" },
  {
    id: 2,
    userText: "is this your best recommendation?",
    replySnippet: "Not quite. After a second pass…",
  },
  { id: 3, userText: "third question" },
];

describe("MessageMinimap", () => {
  it("renders one dash per entry, marks the active one, and jumps on click", () => {
    const onJump = vi.fn();
    render(<MessageMinimap entries={entries} activeId={2} onJump={onJump} />);
    const dashes = screen.getAllByTestId("minimap-dash");
    expect(dashes).toHaveLength(3);
    expect(dashes[1]).toHaveAttribute("aria-current", "true");
    expect(dashes[0]).not.toHaveAttribute("aria-current");
    fireEvent.click(dashes[2]);
    expect(onJump).toHaveBeenCalledWith(3);
  });

  it("shows the preview card on hover with user text and reply snippet", () => {
    render(
      <MessageMinimap entries={entries} activeId={null} onJump={vi.fn()} />,
    );
    const rail = screen.getByTestId("message-minimap");
    // Rail spans 3 × 18px slots; a pointer at y≈27 lands on the middle dash.
    rail.getBoundingClientRect = () =>
      ({
        top: 0,
        height: 54,
        left: 0,
        width: 32,
        bottom: 54,
        right: 32,
        x: 0,
        y: 0,
        toJSON: () => ({}),
      }) as DOMRect;
    fireEvent.pointerMove(rail, { clientY: 27 });
    const card = screen.getByTestId("minimap-preview");
    expect(card).toHaveTextContent("is this your best recommendation?");
    expect(card).toHaveTextContent("Not quite. After a second pass…");
    fireEvent.pointerLeave(rail);
    expect(screen.queryByTestId("minimap-preview")).toBeNull();
  });

  it("scrubs while dragging: press then move fires a jump per dash crossed, once each", () => {
    const onJump = vi.fn();
    render(
      <MessageMinimap entries={entries} activeId={null} onJump={onJump} />,
    );
    const rail = screen.getByTestId("message-minimap");
    rail.getBoundingClientRect = () =>
      ({
        top: 0,
        height: 54,
        left: 0,
        width: 32,
        bottom: 54,
        right: 32,
        x: 0,
        y: 0,
        toJSON: () => ({}),
      }) as DOMRect;
    fireEvent.pointerDown(rail, {
      clientY: 3,
      pointerType: "mouse",
      pointerId: 1,
    });
    expect(onJump).toHaveBeenLastCalledWith(1);
    fireEvent.pointerMove(rail, {
      clientY: 27,
      pointerType: "mouse",
      pointerId: 1,
    });
    expect(onJump).toHaveBeenLastCalledWith(2);
    // wiggling within the same dash does not re-fire
    fireEvent.pointerMove(rail, {
      clientY: 30,
      pointerType: "mouse",
      pointerId: 1,
    });
    expect(onJump).toHaveBeenCalledTimes(2);
    fireEvent.pointerMove(rail, {
      clientY: 50,
      pointerType: "mouse",
      pointerId: 1,
    });
    expect(onJump).toHaveBeenLastCalledWith(3);
    fireEvent.pointerUp(rail, { pointerType: "mouse", pointerId: 1 });
    expect(onJump).toHaveBeenCalledTimes(3);
  });

  it("renders nothing for fewer than two entries", () => {
    render(
      <MessageMinimap
        entries={entries.slice(0, 1)}
        activeId={null}
        onJump={vi.fn()}
      />,
    );
    expect(screen.queryByTestId("message-minimap")).toBeNull();
  });
});
