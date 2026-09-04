"use client";

import { useEffect, useRef, type ReactNode } from "react";
import { useDialogDismiss } from "@/app/shared/ui/useDialogDismiss";

// ConfirmModal — the chat surface's in-app confirm.
//
// The projects surfaces used to reach for window.confirm() for "transfer this
// project", "stop sharing it", and "delete it": the only native confirms in
// the chat UI. A native confirm is unstyled, titles itself with the browser's
// origin, blocks the whole tab, cannot render the project as anything but
// text, and in an installed PWA window it reads as a browser error rather than
// as part of the app.
//
// The treatment is deliberately the one DeleteProjectConfirm already uses —
// --color-surface-1 over the --color-overlay-strong scrim with the standard
// shadow — so every confirm in this pass reads as one family. Kept minimal on
// purpose: a title, arbitrary body content (so a confirm can quote counts or
// name a team), and two buttons.
//
// Escape cancels. It is NOT a focus trap (see useDialogDismiss): focus moves
// into the dialog on mount, which is the part that is cheap and correct
// without a shared dialog primitive for the whole app.
export function ConfirmModal({
  title,
  children,
  confirmLabel,
  cancelLabel = "Cancel",
  danger,
  busy,
  onConfirm,
  onCancel,
}: {
  title: string;
  children?: ReactNode;
  confirmLabel: string;
  cancelLabel?: string;
  // Destructive confirms colour the confirm button, not the cancel.
  danger?: boolean;
  busy?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  useDialogDismiss(true, onCancel);
  const panelRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    panelRef.current?.focus();
  }, []);

  return (
    <div className="fixed inset-0 z-[60] flex items-center justify-center px-4">
      <button
        aria-label={`Cancel: ${title}`}
        className="absolute inset-0 bg-[var(--color-overlay-strong)] backdrop-blur-[2px]"
        type="button"
        onClick={onCancel}
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-label={title}
        className="relative z-10 w-full max-w-[28rem] rounded-[1rem] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] p-5 shadow-[var(--shadow-md)] outline-none"
      >
        <h2 className="mb-2 text-[1rem] font-semibold text-[var(--color-text-primary)]">
          {title}
        </h2>
        {children ? (
          <div className="mb-4 grid gap-[0.4rem] text-[0.85rem] leading-[1.55] text-[var(--color-text-secondary)]">
            {children}
          </div>
        ) : null}
        <div className="flex flex-wrap items-center justify-end gap-2">
          <button
            type="button"
            className="rounded-md px-3 py-1.5 text-[0.8rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)]"
            onClick={onCancel}
          >
            {cancelLabel}
          </button>
          <button
            type="button"
            disabled={busy}
            className={[
              "rounded-md px-3 py-1.5 text-[0.8rem] font-medium transition disabled:opacity-60",
              danger
                ? "text-[var(--color-danger)] hover:bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)]"
                : "bg-[var(--color-accent)] text-[var(--color-surface-1)] hover:opacity-90",
            ].join(" ")}
            onClick={onConfirm}
          >
            {confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}

export default ConfirmModal;
