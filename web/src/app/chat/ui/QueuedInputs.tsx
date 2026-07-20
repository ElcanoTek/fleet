"use client";

// #785: the pending-input chip strip under the composer. Renders the
// conversation's queued follow-ups / steer requests with remove and
// send-now affordances; state arrives via queue.updated on the live stream
// (full snapshots) plus GET /queue refreshes on submit and reconnect.

import { Icon } from "./Icon";
import type { QueuedInput } from "./useTurnStream";

export function QueuedInputs({
  items,
  onRemove,
  onSendNow,
}: {
  items: QueuedInput[];
  onRemove: (inputId: string) => void;
  onSendNow: (inputId: string) => void;
}) {
  if (items.length === 0) return null;
  return (
    <div className="mx-auto mt-2 flex w-full max-w-3xl flex-col gap-1.5 px-1" data-testid="queued-inputs">
      {items.map((it) => (
        <div
          key={it.id}
          className="flex items-center gap-2 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] px-3 py-1.5 text-xs text-[var(--color-text-muted)]"
        >
          <span className="shrink-0 rounded-[var(--radius-pill)] bg-[var(--color-surface-3)] px-2 py-0.5 text-[0.65rem] uppercase tracking-wide">
            {it.state === "injected" ? "steering" : it.mode === "steer" ? "steer" : "queued"}
          </span>
          <span className="min-w-0 flex-1 truncate" title={it.message_preview}>
            {it.message_preview}
          </span>
          {it.state === "queued" ? (
            <>
              <button
                type="button"
                aria-label="Send now"
                data-tip-top="Send now"
                className="shrink-0 rounded p-1 transition hover:bg-[var(--color-status-success-bg)] hover:text-[var(--color-status-success-fg)]"
                onClick={() => onSendNow(it.id)}
              >
                <Icon name="arrow-up" className="size-3" />
              </button>
              <button
                type="button"
                aria-label="Remove from queue"
                data-tip-top="Remove"
                className="shrink-0 rounded p-1 transition hover:bg-[var(--color-status-error-bg)] hover:text-[var(--color-status-error-fg)]"
                onClick={() => onRemove(it.id)}
              >
                <Icon name="close" className="size-3" />
              </button>
            </>
          ) : null}
        </div>
      ))}
    </div>
  );
}
