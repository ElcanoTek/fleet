"use client";

// Modal confirm/alert dialog for the orchestrator view. Replaces moc's
// imperative showConfirm()/showAlert() (modals.js) with a controlled React
// component. Rendered by the dashboard, driven by state.

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
    <div className="modal-overlay is-open" role="dialog" aria-modal="true" aria-label={title}>
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
