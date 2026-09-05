"use client";

import { useCallback, useRef } from "react";
import { useDialogA11y } from "./useDialogA11y";

// Modal confirm/alert dialog for the orchestrator view. Replaces moc's
// imperative showConfirm()/showAlert() (modals.js) with a controlled React
// component. Rendered by the dashboard, driven by state.
//
// This is NOT the chat surface's ConfirmDialog (chat/ui/ConfirmDialog.tsx) and
// it deliberately did not join the B-2 dialog unification. The orchestrator is
// a second design system: its dialogs are the `.modal-overlay` / `.modal` /
// `.btn` CSS classes in globals.css, shared with TaskCreateModal, DatasetsPanel
// and LogViewer, and the stacking rule this file depends on
// (`confirm-overlay` painting above ordinary `.modal-overlay` peers) is one of
// those classes. Rebasing this one dialog onto the Tailwind-token DialogShell
// would leave it looking foreign among its four siblings and would fork the
// orchestrator's overlay stacking across two mechanisms — for no gain, since
// `.modal` is already opaque and so has none of the bleed-through the B-2
// finding is about. If the orchestrator ever moves onto the token surfaces, it
// moves as a set.
//
// What it does share with DialogShell is the keyboard contract, via the
// orchestrator's own useDialogA11y (the New Task modal's hook): focus moves in
// on open — to Cancel, the safe action, so Enter cannot fire the destructive
// one by reflex (an alert has only OK) — Tab is trapped, Escape cancels, and
// focus returns to the opener on close. Before
// this the dialog was aria-modal in name only: Tab walked out behind it and
// Escape did nothing, so a keyboard user had to find the Cancel button.
//
// A caller with an in-flight action passes `busy`: the confirm button disables
// (the label callers already swap to "Deleting…" is not a guard on its own —
// a second click double-submitted) and Escape/Cancel are ignored, matching the
// `if (!busy) setX(null)` guards those callers keep in onCancel.

export type ConfirmDialogProps = {
  open: boolean;
  title?: string;
  message: string;
  confirmLabel?: string;
  cancelLabel?: string;
  // When onCancel is omitted the dialog renders as a single-OK alert.
  onConfirm: () => void;
  onCancel?: () => void;
  // The confirmed action is in flight: disables confirm and ignores dismissal.
  busy?: boolean;
};

export function ConfirmDialog({
  open,
  title = "Confirm",
  message,
  confirmLabel = "OK",
  cancelLabel = "Cancel",
  onConfirm,
  onCancel,
  busy = false,
}: ConfirmDialogProps) {
  const overlayRef = useRef<HTMLDivElement | null>(null);
  const safeActionRef = useRef<HTMLButtonElement | null>(null);
  const isAlert = !onCancel;
  // Escape: cancels a confirm, acknowledges an alert (its only action), and is
  // a no-op while busy — the same rule the Cancel button follows.
  const dismiss = useCallback(() => {
    if (busy) return;
    if (onCancel) onCancel();
    else onConfirm();
  }, [busy, onCancel, onConfirm]);
  useDialogA11y(open, overlayRef, dismiss, { initialFocusRef: safeActionRef });

  if (!open) return null;
  return (
    // confirm-overlay stacks this ABOVE ordinary .modal-overlay peers: the
    // dialog is the terminal decision layer and is summoned from INSIDE other
    // modals (Stop/Delete in the task modal), where equal z-index left DOM
    // order to decide — and the task modal, rendered later, painted over it.
    <div
      ref={overlayRef}
      className="modal-overlay confirm-overlay is-open"
      role="dialog"
      aria-modal="true"
      aria-label={title}
      aria-busy={busy || undefined}
    >
      <div className={`modal ${isAlert ? "alert-modal" : "confirm-modal"}`}>
        <div className="modal-header">
          <h3>{title}</h3>
        </div>
        <div className="modal-body">
          <p>{message}</p>
          <div className="modal-actions">
            {onCancel ? (
              <button
                type="button"
                ref={safeActionRef}
                className="btn btn-secondary"
                onClick={dismiss}
              >
                {cancelLabel}
              </button>
            ) : null}
            <button
              type="button"
              ref={isAlert ? safeActionRef : undefined}
              className="btn btn-primary"
              disabled={busy}
              onClick={onConfirm}
            >
              {confirmLabel}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

export default ConfirmDialog;
