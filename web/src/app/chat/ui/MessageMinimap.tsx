"use client";

import { useRef, useState } from "react";

// MessageMinimap — the Codex-style jump rail at the left edge of the
// transcript: one dash per user message (the most recent MINIMAP_MAX), the
// current one emphasized. Click a dash to jump to that turn; press and drag
// along the rail to scrub through them; hover shows a preview card with the
// user's message and the start of the assistant's reply.
//
// Purely presentational: the parent (ChatTranscript, which owns the
// virtualizer) supplies the entries and performs the actual scrolling via
// onJump. Desktop-only by design — hover has no meaning on touch and a
// press-drag scrub would fight touch scrolling — so the parent hides it
// below the sm breakpoint.

export const MINIMAP_MAX = 10;

export type MinimapEntry = {
  id: number;
  // The user's message text (title of the preview card).
  userText: string;
  // The start of the assistant's reply, when one exists.
  replySnippet?: string;
};

export type MessageMinimapProps = {
  entries: MinimapEntry[];
  activeId: number | null;
  onJump: (id: number) => void;
};

// Vertical rhythm of the rail: each dash owns an 18px slot (dash + gap),
// used both for layout and for mapping a pointer Y to an entry while
// scrubbing.
const DASH_PITCH = 18;

export function MessageMinimap({
  entries,
  activeId,
  onJump,
}: MessageMinimapProps) {
  const railRef = useRef<HTMLDivElement | null>(null);
  const [hoverIdx, setHoverIdx] = useState<number | null>(null);
  const [dragging, setDragging] = useState(false);
  // The last index a drag jumped to, so scrubbing over the same dash doesn't
  // re-fire the jump every pointermove.
  const lastJumpIdx = useRef<number | null>(null);

  if (entries.length < 2) return null;

  const indexFromPointer = (clientY: number): number => {
    const rect = railRef.current?.getBoundingClientRect();
    if (!rect || rect.height <= 0) return 0;
    const t = (clientY - rect.top) / rect.height;
    return Math.min(
      entries.length - 1,
      Math.max(0, Math.floor(t * entries.length)),
    );
  };

  const jumpTo = (idx: number) => {
    if (lastJumpIdx.current === idx) return;
    lastJumpIdx.current = idx;
    onJump(entries[idx].id);
  };

  const onPointerDown = (e: React.PointerEvent<HTMLDivElement>) => {
    // Mouse/pen only — touch keeps native scrolling.
    if (e.pointerType === "touch") return;
    e.preventDefault();
    railRef.current?.setPointerCapture?.(e.pointerId);
    setDragging(true);
    lastJumpIdx.current = null;
    const idx = indexFromPointer(e.clientY);
    setHoverIdx(idx);
    jumpTo(idx);
  };

  const onPointerMove = (e: React.PointerEvent<HTMLDivElement>) => {
    const idx = indexFromPointer(e.clientY);
    setHoverIdx(idx);
    if (dragging) jumpTo(idx);
  };

  const onPointerUp = (e: React.PointerEvent<HTMLDivElement>) => {
    railRef.current?.releasePointerCapture?.(e.pointerId);
    setDragging(false);
    lastJumpIdx.current = null;
  };

  const hovered = hoverIdx != null ? entries[hoverIdx] : null;

  return (
    <nav aria-label="Conversation minimap" className="relative">
      <div
        ref={railRef}
        data-testid="message-minimap"
        className="flex cursor-pointer touch-none flex-col"
        style={{ paddingBlock: 4 }}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerCancel={onPointerUp}
        onPointerLeave={() => {
          if (!dragging) setHoverIdx(null);
        }}
      >
        {entries.map((entry, idx) => {
          const active = entry.id === activeId;
          const hot = idx === hoverIdx;
          return (
            <button
              key={entry.id}
              type="button"
              tabIndex={0}
              aria-label={`Jump to: ${entry.userText.slice(0, 60) || "message"}`}
              aria-current={active ? "true" : undefined}
              data-testid="minimap-dash"
              className="group/dash flex w-8 items-center focus-visible:outline-none"
              style={{ height: DASH_PITCH }}
              onClick={() => {
                lastJumpIdx.current = null;
                onJump(entry.id);
              }}
              onFocus={() => setHoverIdx(idx)}
              onBlur={() => setHoverIdx((h) => (h === idx ? null : h))}
            >
              <span
                aria-hidden="true"
                className={[
                  "h-[2.5px] rounded-full transition-all duration-150",
                  active
                    ? "w-6 bg-[var(--color-text-primary)]"
                    : hot
                      ? "w-5 bg-[var(--color-text-secondary)]"
                      : "w-3 bg-[color-mix(in_srgb,var(--color-text-muted)_45%,transparent)] group-focus-visible/dash:w-5 group-focus-visible/dash:bg-[var(--color-accent)]",
                ].join(" ")}
              />
            </button>
          );
        })}
      </div>

      {hovered ? (
        <div
          role="tooltip"
          data-testid="minimap-preview"
          className="pointer-events-none absolute left-10 z-20 w-72 max-w-[50vw] rounded-xl border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] p-3 shadow-[var(--shadow-md)]"
          style={{
            top: Math.max(0, (hoverIdx ?? 0) * DASH_PITCH - 12),
          }}
        >
          <p className="m-0 line-clamp-2 text-[0.85rem] font-medium text-[var(--color-text-primary)]">
            {hovered.userText || "(empty message)"}
          </p>
          {hovered.replySnippet ? (
            <p className="m-0 mt-1 line-clamp-3 text-[0.78rem] leading-snug text-[var(--color-text-muted)]">
              {hovered.replySnippet}
            </p>
          ) : null}
        </div>
      ) : null}
    </nav>
  );
}

export default MessageMinimap;
