"use client";

import { useEffect, useState } from "react";
import { Icon } from "./Icon";
import { movePinned } from "./conversationOrganization";

// PinnedReorderList is the rail's reorder mode for pinned chats
// (docs/CHAT-PIN-ORDER.md). The Pinned section swaps its ordinary rows for
// this list while the mode is on: each chat can be dragged by its row
// (mouse, pen) or moved with its Move up / Move down buttons (keyboard,
// touch — HTML drag and drop does not fire for touch), and every move is
// announced. Each move hands the whole new order to onReorder, which saves
// it; the list itself holds no order of its own.

// A private drag type, so a reorder drag is never mistaken for the
// chat-into-project drag the ordinary rows start (CONV_DRAG_MIME).
const PIN_ORDER_MIME = "application/x-fleet-pin-order";

export type ReorderableChat = { id: string; title: string };

export function PinnedReorderList({
  chats,
  onReorder,
  onDone,
}: {
  chats: readonly ReorderableChat[];
  onReorder: (ids: string[]) => void;
  onDone: () => void;
}) {
  const [dragId, setDragId] = useState<string | null>(null);
  const [overIndex, setOverIndex] = useState<number | null>(null);
  const [announcement, setAnnouncement] = useState("");
  const ids = chats.map((c) => c.id);

  // Escape leaves the mode, as Done does.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onDone();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onDone]);

  const move = (from: number, to: number) => {
    if (from === to || to < 0 || to >= ids.length) return;
    onReorder(movePinned(ids, from, to));
    const title = chats[from]?.title || "Chat";
    setAnnouncement(`${title} moved to position ${to + 1} of ${ids.length}.`);
  };

  const endDrag = () => {
    setDragId(null);
    setOverIndex(null);
  };

  return (
    <div data-testid="pinned-reorder">
      <p className="px-2 pb-1 text-[0.72rem] leading-snug text-[var(--color-text-secondary)]">
        Drag a chat, or use its arrows, to set the order. Chats you pin later appear at the top.
      </p>
      <ol aria-label="Pinned chats, in order" className="flex flex-col gap-0.5" data-testid="pinned-reorder-list">
        {chats.map((chat, index) => {
          const title = chat.title || "Untitled chat";
          const dropHere = dragId !== null && overIndex === index && dragId !== chat.id;
          return (
            <li key={chat.id} data-testid={`pinned-reorder-item-${chat.id}`}>
              <div
                draggable
                onDragStart={(e) => {
                  e.dataTransfer.setData(PIN_ORDER_MIME, chat.id);
                  e.dataTransfer.effectAllowed = "move";
                  setDragId(chat.id);
                }}
                onDragOver={(e) => {
                  if (!e.dataTransfer.types.includes(PIN_ORDER_MIME)) return;
                  e.preventDefault();
                  e.dataTransfer.dropEffect = "move";
                  if (overIndex !== index) setOverIndex(index);
                }}
                onDrop={(e) => {
                  const id = e.dataTransfer.getData(PIN_ORDER_MIME);
                  e.preventDefault();
                  endDrag();
                  const from = ids.indexOf(id);
                  if (from >= 0) move(from, index);
                }}
                onDragEnd={endDrag}
                className={`flex min-h-9 cursor-grab items-center gap-1.5 rounded-[var(--radius-md)] border px-1.5 py-1 text-[0.85rem] text-[var(--color-text-primary)] active:cursor-grabbing ${
                  dropHere
                    ? "border-[var(--color-primary-fill)] bg-[var(--rail-hover)]"
                    : "border-transparent bg-[var(--color-surface-2)]"
                } ${dragId === chat.id ? "opacity-50" : ""}`}
              >
                <Icon name="selector" className="size-3.5 shrink-0 text-[var(--color-text-muted)]" />
                <span className="min-w-0 flex-1 truncate">{title}</span>
                <button
                  type="button"
                  aria-label={`Move ${title} up`}
                  data-testid={`pinned-reorder-up-${chat.id}`}
                  disabled={index === 0}
                  onClick={() => move(index, index - 1)}
                  className="inline-flex size-7 shrink-0 items-center justify-center rounded-md text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-35 disabled:hover:bg-transparent any-pointer-coarse:size-11"
                >
                  <Icon name="arrow-up" className="size-3.5" />
                </button>
                <button
                  type="button"
                  aria-label={`Move ${title} down`}
                  data-testid={`pinned-reorder-down-${chat.id}`}
                  disabled={index === chats.length - 1}
                  onClick={() => move(index, index + 1)}
                  className="inline-flex size-7 shrink-0 items-center justify-center rounded-md text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-35 disabled:hover:bg-transparent any-pointer-coarse:size-11"
                >
                  <Icon name="arrow-up" className="size-3.5 rotate-180" />
                </button>
              </div>
            </li>
          );
        })}
      </ol>
      <div className="px-2 pt-1.5">
        <button
          type="button"
          data-testid="pinned-reorder-done"
          onClick={onDone}
          className="inline-flex min-h-8 items-center gap-1.5 rounded-md bg-[var(--color-primary-fill)] px-3 text-[0.8rem] font-semibold text-[var(--color-on-primary-fill)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-11"
        >
          <Icon name="check" className="size-3.5" />
          Done
        </button>
      </div>
      <p className="sr-only" role="status" aria-live="polite">
        {announcement}
      </p>
    </div>
  );
}
