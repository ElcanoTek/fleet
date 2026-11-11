import { t as tr } from "@/app/lib/i18n";
import { useEffect, useRef, useState } from "react";
import { Icon } from "./Icon";
import { movePinned } from "./conversationOrganization";

// ChatReorderList is the rail's reorder mode (docs/CHAT-ORDER.md), for
// the Pinned section and for a project's chats. The section swaps its
// ordinary rows for this list while the mode is on: each chat can be dragged by its row
// (mouse, pen) or moved with its Move up / Move down buttons (keyboard,
// touch — HTML drag and drop does not fire for touch), and every move is
// announced. Each move hands the whole new order to onReorder, which saves
// it; the list itself holds no order of its own.

// A private drag type, so a reorder drag is never mistaken for the
// chat-into-project drag the ordinary rows start (CONV_DRAG_MIME).
const CHAT_ORDER_MIME = "application/x-fleet-chat-order";

export type ReorderableChat = { id: string; title: string };

export function ChatReorderList({
  chats,
  label,
  hint,
  onReorder,
  onDone,
}: {
  chats: readonly ReorderableChat[];
  // The list's accessible name ("Pinned chats, in order").
  label: string;
  // One line above the list saying what the order is for.
  hint: string;
  onReorder: (ids: string[]) => void;
  onDone: () => void;
}) {
  const [dragId, setDragId] = useState<string | null>(null);
  const [overIndex, setOverIndex] = useState<number | null>(null);
  const [announcement, setAnnouncement] = useState("");
  const ids = chats.map((c) => c.id);

  const rootRef = useRef<HTMLDivElement | null>(null);

  // Escape leaves the mode, as Done does — and only that: it is taken in the
  // capture phase and stopped, so the rail's own Escape (collapse the
  // overlay, close the sidebar) does not also fire.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      e.stopPropagation();
      e.preventDefault();
      onDone();
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

  const endDrag = () => {
    setDragId(null);
    setOverIndex(null);
  };

  return (
    <div data-testid="chat-reorder" ref={rootRef}>
      <p className="px-2 pb-1 text-[0.72rem] leading-snug text-[var(--color-text-secondary)]">
        {hint}
      </p>
      <ol aria-label={label} className="flex flex-col gap-0.5" data-testid="chat-reorder-list">
        {chats.map((chat, index) => {
          const title = chat.title || tr("Untitled chat");
          const dropHere = dragId !== null && overIndex === index && dragId !== chat.id;
          return (
            <li key={chat.id} data-testid={`chat-reorder-item-${chat.id}`}>
              <div
                draggable
                onDragStart={(e) => {
                  e.dataTransfer.setData(CHAT_ORDER_MIME, chat.id);
                  e.dataTransfer.effectAllowed = "move";
                  setDragId(chat.id);
                }}
                onDragOver={(e) => {
                  if (!e.dataTransfer.types.includes(CHAT_ORDER_MIME)) return;
                  e.preventDefault();
                  e.dataTransfer.dropEffect = "move";
                  if (overIndex !== index) setOverIndex(index);
                }}
                onDrop={(e) => {
                  const id = e.dataTransfer.getData(CHAT_ORDER_MIME);
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
                  aria-label={tr("Move {0} up", [title])}
                  data-testid={`chat-reorder-up-${chat.id}`}
                  disabled={index === 0}
                  onClick={() => {
                  move(index, index - 1);
                  keepFocus(chat.id, "up");
                }}
                  className="inline-flex size-7 shrink-0 items-center justify-center rounded-md text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-35 disabled:hover:bg-transparent any-pointer-coarse:size-11"
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
          data-testid="chat-reorder-done"
          onClick={onDone}
          className="inline-flex min-h-8 items-center gap-1.5 rounded-md bg-[var(--color-primary-fill)] px-3 text-[0.8rem] font-semibold text-[var(--color-on-primary-fill)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-11"
        >
          <Icon name="check" className="size-3.5" />
          {tr("Done")}
        </button>
      </div>
      <p className="sr-only" role="status" aria-live="polite">
        {announcement}
      </p>
    </div>
  );
}
