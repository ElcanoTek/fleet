"use client";

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

export type ConfirmDialogProps = {
  open: boolean;
  title?: string;
  message: string;
  confirmLabel?: string;
  cancelLabel?: string;
  // When onCancel is omitted the dialog renders as a single-OK alert.
  onConfirm: () => void;
  onCancel?: () => void;
};

export function ConfirmDialog({
  open,
  title = "Confirm",
  message,
  confirmLabel = "OK",
  cancelLabel = "Cancel",
  onConfirm,
  onCancel,
}: ConfirmDialogProps) {
  if (!open) return null;
  const isAlert = !onCancel;
  return (
    // confirm-overlay stacks this ABOVE ordinary .modal-overlay peers: the
    // dialog is the terminal decision layer and is summoned from INSIDE other
    // modals (Stop/Delete in the task modal), where equal z-index left DOM
    // order to decide — and the task modal, rendered later, painted over it.
    <div
      className="modal-overlay confirm-overlay is-open"
      role="dialog"
      aria-modal="true"
      aria-label={title}
    >
      <div className={`modal ${isAlert ? "alert-modal" : "confirm-modal"}`}>
        <div className="modal-header">
          <h3>{title}</h3>
        </div>
        <div className="modal-body">
          <p>{message}</p>
          <div className="modal-actions">
            {onCancel ? (
              <button type="button" className="btn btn-secondary" onClick={onCancel}>
                {cancelLabel}
              </button>
            ) : null}
            <button type="button" className="btn btn-primary" onClick={onConfirm}>
              {confirmLabel}
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

export default ConfirmDialog;
