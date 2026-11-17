import { t as tr } from "@/app/lib/i18n";
import { useId, useRef, type RefObject } from "react";
import { createPortal } from "react-dom";
import { CloseButton } from "@/app/shared/ui/CloseButton";
import { DialogShell } from "@/app/shared/ui/DialogShell";
import { Icon } from "@/app/shared/ui/Icon";
import { Button } from "@/components/ui/button";

export type ApprovalModesInfoModalProps = {
  open: boolean;
  onClose: () => void;
  returnFocusRef?: RefObject<HTMLElement | null>;
};

export function ApprovalModesInfoModal({
  open,
  onClose,
  returnFocusRef,
}: ApprovalModesInfoModalProps) {
  const titleId = useId();
  const descId = useId();
  const closeBtnRef = useRef<HTMLButtonElement | null>(null);

  if (!open) return null;

  const modal = (
    <DialogShell
      labelledBy={titleId}
      describedBy={descId}
      scrimLabel={tr("Close approval modes info")}
      onDismiss={onClose}
      layer="topmost"
      initialFocusRef={closeBtnRef}
      returnFocusRef={returnFocusRef}
      testId="approval-modes-info-modal"
      className="flex max-w-[30rem] flex-col overflow-hidden"
    >
      <header className="flex items-start gap-3 border-b border-[var(--color-border)] bg-[var(--gradient-surface-panel)] px-5 py-4">
        <span
          aria-hidden="true"
          className="flex size-9 shrink-0 items-center justify-center rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-2)] text-[var(--color-primary)]"
        >
          <Icon name="alert-circle" className="size-4" />
        </span>
        <div className="min-w-0 flex-1">
          <h2
            id={titleId}
            className="m-0 text-base font-semibold text-[var(--color-text-primary)]"
          >
            {tr("Approval modes")}
          </h2>
          <p
            id={descId}
            className="m-0 mt-0.5 text-xs leading-relaxed text-[var(--color-text-muted)]"
          >
            {tr("Choose how much the assistant can do on its own in this conversation.")}
          </p>
        </div>
        <CloseButton
          label={tr("Close approval modes info")}
          onClick={onClose}
        />
      </header>

      <div className="grid gap-2.5 px-5 py-4 text-[0.8125rem] leading-relaxed">
        <div className="rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] p-3">
          <div className="flex items-center gap-1.5 text-xs font-semibold text-[var(--color-text-primary)]">
            <Icon name="shield" className="size-3.5 shrink-0 text-[var(--color-text-secondary)]" />
            <span>{tr("Manual")}</span>
          </div>
          <p className="m-0 mt-1 text-xs leading-relaxed text-[var(--color-text-secondary)]">
            {tr("Asks for your confirmation on an approval card before every critical action runs.")}
          </p>
        </div>

        <div className="rounded-[var(--radius-md)] border border-[color-mix(in_srgb,var(--color-accent)_40%,var(--color-border))] bg-[color-mix(in_srgb,var(--color-accent)_10%,var(--color-surface-2))] p-3">
          <div className="flex items-center gap-1.5 text-xs font-semibold text-[var(--color-accent)]">
            <Icon name="zap" className="size-3.5 shrink-0" />
            <span>{tr("Auto-approve")}</span>
          </div>
          <p className="m-0 mt-1 text-xs leading-relaxed text-[var(--color-text-secondary)]">
            {tr("Automatically runs routine, reversible workspace actions; external plans, shares, and messages still ask first.")}
          </p>
        </div>

        <div className="rounded-[var(--radius-md)] border border-transparent p-3 [background:linear-gradient(var(--color-warning-tint),var(--color-warning-tint))_padding-box,linear-gradient(135deg,#f59e0b,#ec4899,#8b5cf6)_border-box]">
          <div className="flex items-center gap-1.5 text-xs font-semibold text-[var(--color-warning-text)]">
            <Icon name="zap" className="size-3.5 shrink-0 text-[var(--color-warning-icon)]" />
            <span>{tr("Autonomous")}</span>
          </div>
          <p className="m-0 mt-1 text-xs leading-relaxed text-[var(--color-text-primary)]">
            {tr("Executes live platform actions and eligible prefilled forms (after a 10s countdown) with receipt cards; external messages, calendar invites, schedules, git push, and deployment-blocked actions still require approval.")}
          </p>
        </div>
      </div>

      <footer className="flex items-center justify-end border-t border-[var(--color-border)] px-5 py-3">
        <Button
          ref={closeBtnRef}
          type="button"
          variant="default"
          size="sm"
          data-testid="approval-modes-info-close"
          onClick={onClose}
        >
          {tr("Got it")}
        </Button>
      </footer>
    </DialogShell>
  );

  if (typeof document === "undefined") {
    return modal;
  }
  return createPortal(modal, document.body);
}

export default ApprovalModesInfoModal;
