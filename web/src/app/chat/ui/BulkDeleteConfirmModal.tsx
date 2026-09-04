"use client";

import { useEffect, useState } from "react";
import { DialogShell } from "@/app/shared/ui/DialogShell";

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
  // The heading is also the dialog's accessible name, so it is built once.
  const heading = `Delete ${count} conversation${count === 1 ? "" : "s"}?`;

  return (
    <DialogShell
      label={heading}
      scrimLabel="Close bulk delete confirmation"
      onDismiss={onCancel}
      className="max-w-[26rem] p-5"
    >
      <h2 className="mb-1 text-[1rem] font-semibold text-[var(--color-text-primary)]">
        {heading}
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
            "rounded-full px-4 py-2 text-[0.8125rem] font-medium transition",
            // The foreground travels with the fill. --color-surface-1 is the
            // theme-aware readable foreground on a saturated fill (dark in the
            // dark theme, white in the light one) — white was 2.77:1 on the
            // dark theme's #e08080. The countdown state is a genuinely
            // disabled control, so it takes the muted treatment rather than a
            // high-contrast label on a half-alpha fill.
            ready
              ? "bg-[var(--color-danger)] text-[var(--color-surface-1)] hover:opacity-90"
              : "cursor-not-allowed bg-[var(--color-danger)]/50 text-[var(--color-text-muted)]",
          ].join(" ")}
          onClick={onConfirm}
        >
          {ready ? `Delete ${count}` : `Wait ${remaining}s…`}
        </button>
      </div>
    </DialogShell>
  );
}
