"use client";

import type { ReactNode } from "react";
import { DialogShell } from "@/app/shared/ui/DialogShell";

// ConfirmDialog is the chat surface's one in-app confirmation panel.
//
// It exists because the projects surfaces used `window.confirm` for
// everything they had to ask about — move a team-shared chat into a project
// that isn't team-shared, take a chat out of its project, delete a project,
// transfer it, stop sharing it, delete a team learning. A native confirm is
// the browser's dialog, not the app's: it is unstyled, it titles itself with
// the deployment origin, it blocks the whole tab, it looks broken in a PWA
// window, and — the functional part — it can render only a string and can
// hold only OK/Cancel. So it cannot show the project or the team as a chip,
// and it cannot offer the second action the copy itself promises ("expire
// unless pinned" with nothing to click).
//
// This file used to be TWO components. Two agents in the same pass each built
// a chat-surface confirm — ConfirmDialog (no heading, body-as-accessible-name,
// a secondary-action slot) and ConfirmModal (a required title, free-form body,
// a busy state) — neither aware of the other. They are folded here: the title
// is optional, the body is free-form, the secondary action is optional. Both
// shapes are still reachable, and no confirm's copy changed.
//
// Two shapes, one primitive:
//   • titled     — pass `title`; it is the accessible name (h2 + aria-label).
//   • untitled   — pass `bodyId`; the body copy IS the accessible name
//                  (aria-labelledby). Copy that was written as a confirm
//                  question already reads as one, and inventing a heading for
//                  it would be new copy.
//
// The surface (opaque --color-surface-1 over the --color-overlay-strong
// scrim), Escape-to-dismiss, click-outside, and focus-on-open all come from
// the shared DialogShell — see that file for why the panel is opaque.
export function ConfirmDialog({
  bodyId,
  busy,
  cancelLabel = "Cancel",
  cancelAriaLabel,
  children,
  confirmLabel,
  confirmTone = "default",
  layer,
  onCancel,
  onConfirm,
  secondary,
  testId,
  title,
  titleContent,
}: {
  // Wires the body copy up as the dialog's accessible name. Pass this OR
  // `title`.
  bodyId?: string;
  // Disables the confirm while an in-flight action settles.
  busy?: boolean;
  cancelLabel?: string;
  // The scrim is a button (click-outside cancels), so it needs its own label.
  // Defaults to "Cancel: <title>" for a titled confirm.
  cancelAriaLabel?: string;
  children?: ReactNode;
  confirmLabel: string;
  // "default" is the neutral filled button; "accent" is the brand-filled one
  // the transfer confirm shipped with; "danger" tints a destructive confirm
  // and never the cancel beside it.
  confirmTone?: "default" | "accent" | "danger";
  // "stacked" for a confirm summoned from inside another dialog.
  layer?: "modal" | "stacked";
  onCancel: () => void;
  onConfirm: () => void;
  // An optional second way forward — not a second confirm, a different
  // outcome (e.g. "pin it first, then remove it").
  secondary?: { label: string; onClick: () => void };
  testId?: string;
  title?: string;
  // Rich visual title; the plain title remains the dialog and scrim name.
  titleContent?: ReactNode;
}) {
  return (
    <DialogShell
      label={title}
      labelledBy={title ? undefined : bodyId}
      scrimLabel={cancelAriaLabel ?? (title ? `Cancel: ${title}` : "Cancel")}
      onDismiss={onCancel}
      layer={layer}
      className="max-w-[28rem] p-5"
      testId={testId}
    >
      {title ? (
        <h2
          className="mb-2 text-[1rem] font-semibold text-[var(--color-text-primary)]"
        >
          {titleContent ?? title}
        </h2>
      ) : null}
      {children ? (
        // Keep inline copy in normal text flow. A grid turns text fragments
        // and chips into separate rows, leaving punctuation on its own line.
        // Multi-paragraph confirms get spacing only between their paragraphs.
        <div
          id={title ? undefined : bodyId}
          className="mb-4 text-[0.875rem] leading-[1.6] text-[var(--color-text-secondary)] [&>p+p]:mt-[0.4rem]"
        >
          {children}
        </div>
      ) : null}
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
          disabled={busy}
          className={[
            "rounded-md px-3 py-1.5 text-[0.8rem] font-medium transition disabled:opacity-60",
            confirmTone === "danger"
              ? "text-[var(--color-danger)] hover:bg-[color-mix(in_srgb,var(--color-danger)_10%,transparent)]"
              : confirmTone === "accent"
                ? "bg-[var(--color-accent)] text-[var(--color-surface-1)] hover:opacity-90"
                : "bg-[var(--color-text-primary)] text-[var(--color-surface-1)] hover:opacity-80",
          ].join(" ")}
          onClick={onConfirm}
        >
          {confirmLabel}
        </button>
      </div>
    </DialogShell>
  );
}

// NameChip renders a project name (or any short identity) inline in dialog
// copy — the thing a native confirm could only put in curly quotes. The team
// variant takes the two-people glyph as a leading icon so the audience reads
// the same way it does on the rail rows and the project home.
export function NameChip({
  children,
  icon,
  suffix,
}: {
  children: ReactNode;
  icon?: ReactNode;
  // Keep sentence punctuation outside the badge but on the same line.
  suffix?: string;
}) {
  return (
    <span className="whitespace-nowrap">
      <span data-name-chip className="mx-0.5 inline-flex max-w-[14rem] items-center gap-1 rounded-full border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-2 py-0.5 align-baseline text-[0.78rem] font-normal text-[var(--color-text-primary)]">
        {icon}
        <span className="min-w-0 truncate">{children}</span>
      </span>{suffix}
    </span>
  );
}

export default ConfirmDialog;
