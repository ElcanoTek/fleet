import { t as tr } from "@/app/lib/i18n";
import { useEffect, useRef, useState } from "react";
import { Icon } from "./Icon";
import { dragTargetIndex, movePinned } from "./conversationOrganization";

// ChatReorderList is the rail's reorder mode (docs/CHAT-ORDER.md), for
// the Pinned section and for a project's chats. The section swaps its
// ordinary rows for this list while the mode is on. A chat is moved by
// dragging it — with a finger by its handle, with a mouse or pen by any part
// of its row — or one place at a time with its Move up / Move down buttons
// (the keyboard's way, and a screen reader's). Every move is announced. Each
// move hands the whole new order to onReorder, which saves it; the list
// itself holds no order of its own.
//
// The drag is built on pointer events, not HTML drag and drop: that API does
// not fire for touch at all, which is why the list could not be dragged on a
// phone. A finger starts a drag only on the handle (touch-action: none
// there), so touching anywhere else on a row still scrolls the list.

export type ReorderableChat = { id: string; title: string };

// How close to the scroller's top or bottom edge (px) a drag starts scrolling
// it, and how fast (px per frame at the edge itself).
const EDGE = 44;
const EDGE_SPEED = 12;

type Drag = {
  id: string;
  from: number;
  // Where the dragged row would land now.
  to: number;
  // How far the row has been carried, in list pixels (pointer travel plus
  // whatever the list scrolled under it).
  dy: number;
  // The distance between two rows' tops.
  step: number;
};

function scrollParent(el: HTMLElement | null): HTMLElement | null {
  for (let node = el?.parentElement ?? null; node; node = node.parentElement) {
    const overflow = getComputedStyle(node).overflowY;
    if (overflow === "auto" || overflow === "scroll") return node;
  }
  return null;
}

// ReorderDoneButton leaves the mode. It sits above the list, not under it,
// so it is in reach however long the list is: beside the hint, or — where
// the section has a heading row of its own (Pinned) — in that row, where
// Reorder was.
export function ReorderDoneButton({ onDone }: { onDone: () => void }) {
  return (
    <button
      type="button"
      data-testid="chat-reorder-done"
      onClick={onDone}
      className="hit-area-touch ml-auto inline-flex h-6 shrink-0 items-center gap-1 rounded-md bg-[var(--color-primary-fill)] px-2 text-[0.68rem] font-semibold normal-case tracking-normal text-[var(--color-on-primary-fill)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:h-8 any-pointer-coarse:px-2.5"
    >
      <Icon name="check" className="size-3" />
      {tr("Done")}
    </button>
  );
}

export function ChatReorderList({
  chats,
  label,
  hint,
  onReorder,
  onDone,
  doneInHeading = false,
}: {
  chats: readonly ReorderableChat[];
  // The list's accessible name ("Pinned chats, in order").
  label: string;
  // One line above the list saying what the order is for.
  hint: string;
  onReorder: (ids: string[]) => void;
  onDone: () => void;
  // The caller renders ReorderDoneButton in its own heading row.
  doneInHeading?: boolean;
}) {
  const [drag, setDrag] = useState<Drag | null>(null);
  const [announcement, setAnnouncement] = useState("");
  const ids = chats.map((c) => c.id);

  const rootRef = useRef<HTMLDivElement | null>(null);
  const listRef = useRef<HTMLOListElement | null>(null);
  // The drag's fixed facts and its latest pointer position, read by the
  // pointer handlers and the edge-scroll loop without waiting for a render.
  const live = useRef<{
    pointerId: number;
    startY: number;
    lastY: number;
    scroller: HTMLElement | null;
    startScroll: number;
    frame: number | null;
  } | null>(null);
  const dragRef = useRef<Drag | null>(null);
  const setDragBoth = (next: Drag | null) => {
    dragRef.current = next;
    setDrag(next);
  };

  const stopLoop = () => {
    const l = live.current;
    if (l && l.frame !== null) cancelAnimationFrame(l.frame);
    if (l) l.frame = null;
  };
  const cancelDrag = () => {
    stopLoop();
    live.current = null;
    setDragBoth(null);
  };
  // A drag never outlives the list.
  useEffect(() => stopLoop, []);

  // Escape leaves the mode, as Done does — and only that: it is taken in the
  // capture phase and stopped, so the rail's own Escape (collapse the
  // overlay, close the sidebar) does not also fire. During a drag it puts
  // the row back instead, and the mode stays.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      e.stopPropagation();
      e.preventDefault();
      if (dragRef.current) cancelDrag();
      else onDone();
    };
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, [onDone]);

  // Entering the mode puts the focus on the first control in the list, so a
  // keyboard user lands where the work is rather than on the page body.
  useEffect(() => {
    rootRef.current?.querySelector<HTMLButtonElement>("ol button:not([disabled])")?.focus();
  }, []);

  // After an arrow move the moved chat's same arrow keeps the focus; at an end
  // (that arrow now disabled) the focus moves to its other arrow.
  const keepFocus = (id: string, dir: "up" | "down") => {
    requestAnimationFrame(() => {
      const root = rootRef.current;
      if (!root) return;
      const same = root.querySelector<HTMLButtonElement>(`[data-testid="chat-reorder-${dir}-${id}"]`);
      const other = root.querySelector<HTMLButtonElement>(
        `[data-testid="chat-reorder-${dir === "up" ? "down" : "up"}-${id}"]`,
      );
      (same && !same.disabled ? same : other)?.focus();
    });
  };

  const move = (from: number, to: number) => {
    if (from === to || to < 0 || to >= ids.length) return;
    onReorder(movePinned(ids, from, to));
    const title = chats[from]?.title || tr("Chat");
    setAnnouncement(tr("{0} moved to position {1} of {2}.", [title, to + 1, ids.length]));
  };

  // Recompute where the dragged row is from the latest pointer position and
  // the list's scroll, and scroll the list while the pointer sits at an edge.
  const track = () => {
    const l = live.current;
    const d = dragRef.current;
    if (!l || !d) return;
    const scrolled = l.scroller ? l.scroller.scrollTop - l.startScroll : 0;
    // The carried row never leaves the list: past the first or last place it
    // waits there. Carried further it would stretch the scroller's own
    // height, and a drag held at the bottom edge would scroll for ever.
    const dy = Math.max(
      -d.from * d.step,
      Math.min((ids.length - 1 - d.from) * d.step, l.lastY - l.startY + scrolled),
    );
    const to = dragTargetIndex(d.from, dy, d.step, ids.length);
    if (dy !== d.dy || to !== d.to) setDragBoth({ ...d, dy, to });
  };
  const edgeScroll = () => {
    const l = live.current;
    if (!l || !dragRef.current) return;
    l.frame = null;
    const s = l.scroller;
    if (!s) return;
    const r = s.getBoundingClientRect();
    const speed =
      l.lastY < r.top + EDGE
        ? -EDGE_SPEED * Math.min(1, (r.top + EDGE - l.lastY) / EDGE)
        : l.lastY > r.bottom - EDGE
          ? EDGE_SPEED * Math.min(1, (l.lastY - (r.bottom - EDGE)) / EDGE)
          : 0;
    if (speed === 0) return;
    // The row is already in the first or last place: scrolling on would only
    // carry the list away from it, into whatever the scroller holds beyond.
    const d = dragRef.current;
    if (d && ((speed > 0 && d.dy >= (ids.length - 1 - d.from) * d.step) || (speed < 0 && d.dy <= -d.from * d.step))) {
      return;
    }
    const before = s.scrollTop;
    s.scrollTop = before + speed;
    if (s.scrollTop === before) return; // at the end: nothing left to scroll
    track();
    l.frame = requestAnimationFrame(edgeScroll);
  };

  const startDrag = (e: React.PointerEvent<HTMLElement>, id: string, index: number) => {
    if (e.button !== 0 || live.current || ids.length < 2) return;
    const items = listRef.current?.children;
    const first = items?.[0] as HTMLElement | undefined;
    const second = items?.[1] as HTMLElement | undefined;
    if (!first || !second) return;
    const step = second.getBoundingClientRect().top - first.getBoundingClientRect().top;
    if (!(step > 0)) return;
    const scroller = scrollParent(listRef.current);
    live.current = {
      pointerId: e.pointerId,
      startY: e.clientY,
      lastY: e.clientY,
      scroller,
      startScroll: scroller?.scrollTop ?? 0,
      frame: null,
    };
    // The row keeps receiving the pointer wherever it goes, including off
    // the list and off the sidebar.
    try {
      e.currentTarget.setPointerCapture(e.pointerId);
    } catch {
      // No capture (an old engine, a synthetic event): the row's own
      // pointermove still follows a pointer that stays over it.
    }
    e.preventDefault();
    setDragBoth({ id, from: index, to: index, dy: 0, step });
  };
  const onPointerMove = (e: React.PointerEvent<HTMLElement>) => {
    const l = live.current;
    if (!l || e.pointerId !== l.pointerId) return;
    l.lastY = e.clientY;
    track();
    if (l.frame === null) l.frame = requestAnimationFrame(edgeScroll);
  };
  const onPointerUp = (e: React.PointerEvent<HTMLElement>) => {
    const l = live.current;
    const d = dragRef.current;
    if (!l || e.pointerId !== l.pointerId) return;
    cancelDrag();
    if (d) move(d.from, d.to);
  };
  const onPointerCancel = (e: React.PointerEvent<HTMLElement>) => {
    if (live.current && e.pointerId === live.current.pointerId) cancelDrag();
  };

  // While a row is carried, the rows between where it left and where it would
  // land step aside by one place, so the gap shows where a drop puts it.
  const shiftFor = (index: number): number => {
    if (!drag || index === drag.from) return 0;
    if (drag.to > drag.from && index > drag.from && index <= drag.to) return -drag.step;
    if (drag.to < drag.from && index >= drag.to && index < drag.from) return drag.step;
    return 0;
  };

  const arrowClass =
    "inline-flex size-7 shrink-0 items-center justify-center rounded-md text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-35 disabled:hover:bg-transparent any-pointer-coarse:size-11";

  return (
    <div data-testid="chat-reorder" ref={rootRef}>
      <div className="flex items-start gap-2 px-2 pb-1.5">
        <p className="m-0 min-w-0 flex-1 text-[0.72rem] leading-snug text-[var(--color-text-secondary)]">{hint}</p>
        {doneInHeading ? null : <ReorderDoneButton onDone={onDone} />}
      </div>
      <ol
        ref={listRef}
        aria-label={label}
        className={`flex flex-col gap-0.5 ${drag ? "select-none" : ""}`}
        data-testid="chat-reorder-list"
      >
        {chats.map((chat, index) => {
          const title = chat.title || tr("Untitled chat");
          const dragging = drag?.id === chat.id;
          const shift = shiftFor(index);
          return (
            <li
              key={chat.id}
              data-testid={`chat-reorder-item-${chat.id}`}
              data-dragging={dragging ? "" : undefined}
              // The carried row follows the pointer exactly; the others
              // glide into place (not for a person who asked for less motion).
              style={
                dragging
                  ? { transform: `translateY(${drag.dy}px)`, position: "relative", zIndex: 1 }
                  : drag
                    ? { transform: `translateY(${shift}px)` }
                    : undefined
              }
              className={drag && !dragging ? "motion-safe:transition-transform motion-safe:duration-150" : ""}
            >
              {/* The row is a drag surface for a mouse or pen; a finger drags
                  by the handle only. Its buttons are the keyboard's way. */}
              <div
                data-reorder-row=""
                onPointerDown={(e) => {
                  if (e.pointerType === "touch") return;
                  if ((e.target as HTMLElement).closest("button")) return;
                  startDrag(e, chat.id, index);
                }}
                onPointerMove={onPointerMove}
                onPointerUp={onPointerUp}
                onPointerCancel={onPointerCancel}
                className={`flex min-h-9 cursor-grab items-center gap-1 rounded-[var(--radius-md)] border py-0.5 pl-0.5 pr-1 text-[0.85rem] text-[var(--color-text-primary)] any-pointer-coarse:min-h-11 ${
                  dragging
                    ? "cursor-grabbing border-[var(--color-primary-fill)] bg-[var(--color-surface-2)] shadow-[var(--shadow-md)]"
                    : "border-transparent bg-[var(--color-surface-2)]"
                }`}
              >
                <span
                  data-testid={`chat-reorder-handle-${chat.id}`}
                  aria-hidden="true"
                  onPointerDown={(e) => {
                    if (e.pointerType !== "touch") return;
                    startDrag(e, chat.id, index);
                  }}
                  onPointerMove={onPointerMove}
                  onPointerUp={onPointerUp}
                  onPointerCancel={onPointerCancel}
                  // touch-action none: a finger on the handle drags the row
                  // instead of scrolling the list.
                  className="inline-flex h-8 w-7 shrink-0 touch-none items-center justify-center text-[var(--color-text-muted)] any-pointer-coarse:h-10 any-pointer-coarse:w-9"
                >
                  <Icon name="selector" className="size-3.5" />
                </span>
                <span className="min-w-0 flex-1 truncate">{title}</span>
                <button
                  type="button"
                  aria-label={tr("Move {0} up", [title])}
                  data-testid={`chat-reorder-up-${chat.id}`}
                  disabled={index === 0}
                  onClick={() => {
                    move(index, index - 1);
                    keepFocus(chat.id, "up");
                  }}
                  className={arrowClass}
                >
                  <Icon name="arrow-up" className="size-3.5" />
                </button>
                <button
                  type="button"
                  aria-label={tr("Move {0} down", [title])}
                  data-testid={`chat-reorder-down-${chat.id}`}
                  disabled={index === chats.length - 1}
                  onClick={() => {
                    move(index, index + 1);
                    keepFocus(chat.id, "down");
                  }}
                  className={arrowClass}
                >
                  <Icon name="arrow-up" className="size-3.5 rotate-180" />
                </button>
              </div>
            </li>
          );
        })}
      </ol>
      <p className="sr-only" role="status" aria-live="polite">
        {announcement}
      </p>
    </div>
  );
}
