import { t as tr } from "@/app/lib/i18n";
import { useId, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { CloseButton } from "@/app/shared/ui/CloseButton";
import { Icon } from "@/app/shared/ui/Icon";
import { useDialogA11y } from "@/app/shared/ui/useDialogA11y";
import { errorMessage } from "@/app/shared/lib/errorMessage";
import { Button } from "@/components/ui/button";

export type AutonomousRiskModalProps = {
  open: boolean;
  onAccepted: () => void;
  onClose: () => void;
};

export function AutonomousRiskModal({
  open,
  onAccepted,
  onClose,
}: AutonomousRiskModalProps) {
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const titleId = useId();
  const descId = useId();
  const modalRef = useRef<HTMLDivElement | null>(null);
  const acceptBtnRef = useRef<HTMLButtonElement | null>(null);

  useDialogA11y(open, modalRef, onClose, {
    initialFocusRef: acceptBtnRef,
  });

  if (!open) return null;

  const handleAccept = async () => {
    setSubmitting(true);
    setError(null);
    try {
      const res = await fetch("/api/chat/autonomous-accept", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({}),
      });
      if (!res.ok) {
        throw new Error(tr("HTTP {0}", [res.status]));
      }
      onAccepted();
    } catch (err) {
      setError(
        errorMessage(
          err,
          tr("Could not save your Autonomous mode confirmation. Try again."),
        ),
      );
    } finally {
      setSubmitting(false);
    }
  };

  const modal = (
    <div className="scrim fixed inset-0 z-[1300] flex items-center justify-center p-4">
      <button
        type="button"
        tabIndex={-1}
        aria-label={tr("Close Autonomous mode confirmation")}
        onClick={onClose}
        className="absolute inset-0 cursor-default"
      />
      <div
        ref={modalRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-describedby={descId}
        data-testid="autonomous-risk-modal"
        className="motion-safe:animate-pop-up-base relative z-10 flex w-full max-w-[32rem] flex-col overflow-hidden rounded-[1.25rem] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] shadow-[var(--shadow-lg)] outline-none"
      >
        <header className="flex items-start gap-3 border-b border-[var(--color-border)] bg-[var(--gradient-surface-panel)] px-5 py-4">
          <span
            aria-hidden="true"
            className="flex size-9 shrink-0 items-center justify-center rounded-[var(--radius-md)] border border-[var(--color-warning-border)] bg-[var(--color-warning-tint)] text-[var(--color-warning-icon)]"
          >
            <Icon name="zap" className="size-4" />
          </span>
          <div className="min-w-0 flex-1">
            <h2
              id={titleId}
              className="m-0 text-base font-semibold text-[var(--color-text-primary)]"
            >
              {tr("Enable Autonomous mode")}
            </h2>
            <p
              id={descId}
              className="m-0 mt-0.5 text-xs leading-relaxed text-[var(--color-text-muted)]"
            >
              {tr(
                "Autonomous mode lets the assistant execute live platform actions in your chats and patrols without waiting for approval cards.",
              )}
            </p>
          </div>
          <CloseButton
            label={tr("Close Autonomous mode confirmation")}
            onClick={onClose}
          />
        </header>

        <div className="grid gap-3 px-5 py-4 text-[0.8125rem] leading-relaxed text-[var(--color-text-secondary)]">
          <div className="rounded-[var(--radius-md)] border border-[var(--color-warning-border)] bg-[var(--color-warning-tint)] p-3">
            <div className="flex items-center gap-1.5 text-xs font-semibold text-[var(--color-warning-text)]">
              <Icon name="zap" className="size-3.5 shrink-0 text-[var(--color-warning-icon)]" />
              <span>{tr("Runs without asking (recorded with a receipt card)")}</span>
            </div>
            <p className="m-0 mt-1 text-xs leading-relaxed text-[var(--color-text-primary)]">
              {tr(
                "Bundle-configured connector actions (unless blocked by your deployment), optional connector activations, self-addressed owner emails, and eligible prefilled forms (after a 10s countdown) run without asking.",
              )}
            </p>
          </div>

          <div className="rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] p-3">
            <div className="flex items-center gap-1.5 text-xs font-semibold text-[var(--color-text-primary)]">
              <Icon name="shield" className="size-3.5 shrink-0 text-[var(--color-accent)]" />
              <span>{tr("Still requires approval")}</span>
            </div>
            <p className="m-0 mt-1 text-xs leading-relaxed text-[var(--color-text-secondary)]">
              {tr(
                "Built-in external emails, Microsoft 365 messages and calendar invites, recurring task and patrol schedules, git push, and any connector actions your deployment marks as blocked still require approval.",
              )}
            </p>
          </div>

          <p className="m-0 text-xs text-[var(--color-text-muted)]">
            {tr(
              "You can return to Manual in one click at any time using the amber Autonomous pill in the composer.",
            )}
          </p>

          {error ? (
            <p
              role="alert"
              data-testid="autonomous-risk-error"
              className="m-0 rounded-[var(--radius-sm)] border border-[var(--color-danger)] bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)] px-3 py-2 text-xs text-[var(--color-danger)]"
            >
              {error}
            </p>
          ) : null}
        </div>

        <footer className="flex flex-wrap items-center justify-end gap-2 border-t border-[var(--color-border)] px-5 py-3">
          <Button
            type="button"
            variant="secondary"
            size="sm"
            disabled={submitting}
            data-testid="autonomous-risk-cancel"
            onClick={onClose}
          >
            {tr("Cancel")}
          </Button>
          <Button
            ref={acceptBtnRef}
            type="button"
            variant="default"
            size="sm"
            disabled={submitting}
            data-testid="autonomous-risk-accept"
            onClick={() => void handleAccept()}
          >
            {submitting
              ? tr("Enabling...")
              : tr("Accept risk and enable Autonomous")}
          </Button>
        </footer>
      </div>
    </div>
  );

  if (typeof document === "undefined") {
    return modal;
  }
  return createPortal(modal, document.body);
}

export default AutonomousRiskModal;
