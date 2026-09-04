"use client";

import { useEffect, useRef, type ReactNode } from "react";
import { useDialogDismiss } from "@/app/shared/ui/useDialogDismiss";

// ConfirmDialog is the chat surface's in-app confirmation panel.
//
// It exists because three paths on this surface (move a team-shared chat into
// a project that isn't team-shared, take a chat out of its project, delete a
// project from the rail kebab) used `window.confirm`. A
// native confirm is the browser's dialog, not the app's: it is unstyled, it
// titles itself with the deployment origin, it blocks the whole tab, it looks
// broken in a PWA window, and — the functional part — it can render only a
// string and can hold only OK/Cancel. So it cannot show the project or the
// team as a chip, and it cannot offer the second action the copy itself
// promises ("expire unless pinned" with nothing to click).
//
// The treatment is deliberately the one the rest of the pass already uses (see
// ProjectHome's delete-project dialog): a --color-surface-1 panel with
// --shadow-md over a --color-overlay-strong scrim. A later pass unifies every
// dialog surface in the app; this is not a new visual language.
//
// There is no visible heading. The body carries copy that was written as a
// confirm question and reads as one; inventing a title would be new copy, so
// the body IS the accessible name (aria-labelledby).
//
// Escape cancels and focus lands in the panel on mount — the native confirm
// answered Escape, so a replacement that didn't would be a regression for a
// keyboard user. Not a focus trap, matching the house pattern (see
// useDialogDismiss). A later pass folds this and ConfirmModal into one
// primitive; this one carries a secondary action and a body-as-accessible-name
// variant that ConfirmModal has no slot for yet.
export function ConfirmDialog({
  bodyId,
  cancelLabel = "Cancel",
  cancelAriaLabel,
  children,
  confirmLabel,
  confirmTone = "default",
  onCancel,
  onConfirm,
  secondary,
  testId,
}: {
  // bodyId wires the body copy up as the dialog's accessible name.
  bodyId: string;
  cancelLabel?: string;
  // The scrim is a button (click-outside cancels), so it needs its own label.
  cancelAriaLabel: string;
  children: ReactNode;
  confirmLabel: string;
  confirmTone?: "default" | "danger";
  onCancel: () => void;
  onConfirm: () => void;
  // An optional second way forward — not a second confirm, a different
  // outcome (e.g. "pin it first, then remove it").
  secondary?: { label: string; onClick: () => void };
  testId?: string;
}) {
  useDialogDismiss(true, onCancel);
  const panelRef = useRef<HTMLDivElement | null>(null);
  useEffect(() => {
    panelRef.current?.focus();
  }, []);

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center px-4">
      <button
        aria-label={cancelAriaLabel}
        className="absolute inset-0 bg-[var(--color-overlay-strong)] backdrop-blur-[2px]"
        type="button"
        onClick={onCancel}
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-labelledby={bodyId}
        data-testid={testId}
        className="relative z-10 w-full max-w-[28rem] rounded-[1rem] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] p-5 shadow-[var(--shadow-md)] outline-none"
      >
        <p
          id={bodyId}
          className="mb-4 text-[0.875rem] leading-[1.6] text-[var(--color-text-secondary)]"
        >
          {children}
        </p>
        <div className="flex flex-wrap items-center justify-end gap-2">
          <button
            type="button"
            className="rounded-md px-3 py-1.5 text-[0.8rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
            onClick={onCancel}
          >
            {cancelLabel}
          </button>
          {secondary ? (
            <button
              type="button"
              className="rounded-md border border-[var(--color-border-strong)] px-3 py-1.5 text-[0.8rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
              onClick={secondary.onClick}
            >
              {secondary.label}
            </button>
          ) : null}
          <button
            type="button"
            className={[
              "rounded-md px-3 py-1.5 text-[0.8rem] font-medium transition",
              confirmTone === "danger"
                ? "text-[var(--color-danger)] hover:bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)]"
                : "bg-[var(--color-text-primary)] text-[var(--color-surface-1)] hover:opacity-80",
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

// NameChip renders a project name (or any short identity) inline in dialog
// copy — the thing a native confirm could only put in curly quotes. The team
// variant takes the two-people glyph as a leading icon so the audience reads
// the same way it does on the rail rows and the project home.
export function NameChip({
  children,
  icon,
}: {
  children: ReactNode;
  icon?: ReactNode;
}) {
  return (
    <span className="mx-0.5 inline-flex max-w-[14rem] items-center gap-1 truncate rounded-full border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2 py-0.5 align-baseline text-[0.78rem] text-[var(--color-text-primary)]">
      {icon}
      {children}
    </span>
  );
}
