"use client";

import { useEffect, useState } from "react";

// BulkDeleteConfirmModal is the multi-select bulk-delete confirmation (#279).
// It shows the exact selection count and disables the confirm button for a
// 3-second window (with a visible countdown) so an impulsive bulk wipe can't
// fire the instant the modal opens. Cancel is always available.
//
// The parent mounts this component only while the modal is open, so the
// countdown state initializes fresh (to COUNTDOWN_SECONDS) on each open — no
// reset logic is needed inside an effect, and there's no cascading-render
// hazard.
const COUNTDOWN_SECONDS = 3;

export function BulkDeleteConfirmModal({
  count,
  onCancel,
  onConfirm,
}: {
  count: number;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const [remaining, setRemaining] = useState(COUNTDOWN_SECONDS);

  useEffect(() => {
    const start = Date.now();
    const id = window.setInterval(() => {
      const elapsed = (Date.now() - start) / 1000;
      setRemaining(Math.max(0, Math.ceil(COUNTDOWN_SECONDS - elapsed)));
    }, 100);
    return () => window.clearInterval(id);
  }, []);

  const ready = remaining <= 0;

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center px-4">
      <button
        aria-label="Close bulk delete confirmation"
        className="absolute inset-0 bg-[var(--color-overlay-strong)] backdrop-blur-[2px]"
        type="button"
        onClick={onCancel}
      />
      <div className="relative z-10 w-full max-w-[26rem] rounded-[1.25rem] border border-[var(--color-border-strong)] bg-[color-mix(in_srgb,var(--composer-surface)_94%,black)] p-5 shadow-[var(--composer-shadow)] backdrop-blur-sm">
        <h2 className="mb-1 text-[1rem] font-semibold text-[var(--color-text-primary)]">
          Delete {count} conversation{count === 1 ? "" : "s"}?
        </h2>
        <p className="mb-4 text-[0.875rem] leading-[1.6] text-[var(--color-text-secondary)]">
          {count} conversation{count === 1 ? "" : "s"} will be removed. This cannot be
          undone.
        </p>
        <div className="flex items-center justify-end gap-2">
          <button
            type="button"
            className="rounded-full border border-[var(--color-border-strong)] px-4 py-2 text-[0.8125rem] font-medium text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
            onClick={onCancel}
          >
            Cancel
          </button>
          <button
            type="button"
            disabled={!ready}
            className={[
              "rounded-full px-4 py-2 text-[0.8125rem] font-medium text-white transition",
              ready
                ? "bg-[var(--color-danger)] hover:opacity-90"
                : "cursor-not-allowed bg-[var(--color-danger)]/50",
            ].join(" ")}
            onClick={onConfirm}
          >
            {ready ? `Delete ${count}` : `Wait ${remaining}s…`}
          </button>
        </div>
      </div>
    </div>
  );
}
