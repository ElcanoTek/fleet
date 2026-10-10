"use client";

// #785: the pending-input chip strip under the composer. Renders the
// conversation's queued follow-ups / steer requests with remove and
// send-now affordances; state arrives via queue.updated on the live stream
// (full snapshots) plus GET /queue refreshes on submit and reconnect.

import { Icon } from "./Icon";
import type { QueuedInput } from "./useTurnStream";

// The chip's badge. A "resume" row is the turn fleet starts on its own after
// an approval card is settled, waiting behind the running turn; Remove drops
// it like any queued input.
export function queuedInputLabel(it: QueuedInput): string {
  if (it.state === "injected") return "steering";
  if (it.mode === "steer") return "steer";
  if (it.mode === "resume") return "auto-continue";
  return "queued";
}

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
            {queuedInputLabel(it)}
          </span>
          <span className="min-w-0 flex-1 truncate" title={it.message_preview}>
            {it.mode === "resume"
              ? // fleet's own input (docs/RESUME-AFTER-APPROVAL.md): say what
                // it is rather than show its machine-oriented text.
                "Continue after the approval, once the current turn ends"
              : it.message_preview}
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
