"use client";

import {
  useCallback,
  useEffect,
  useRef,
  type ReactNode,
  type RefObject,
} from "react";
import { useDialogDismiss } from "./useDialogDismiss";

// DialogShell — the one modal-dialog base for the chat and settings surfaces.
//
// Every centred dialog in those two surfaces used to hand-roll its own
// overlay, and half of them hand-rolled it wrong: the panel was
// `bg-[color-mix(in_srgb,var(--composer-surface)_94%,black)]`, and
// --composer-surface is a *gradient*, so the color-mix() never resolves, the
// background declaration is dropped, and the "panel" is transparent. The chat
// behind it read straight through the Memories modal, the Shortcuts overlay
// and four confirms — body copy competing with a dense message or an email
// body underneath. That is why the panel surface, the shadow and the radius
// live HERE and are not props: the fix has to be un-reinventable.
//
// What the shell owns: the fixed overlay, the click-to-dismiss scrim (a real
// <button>, so "click outside" is reachable from the keyboard and its
// accessible name says what it does), the OPAQUE --color-surface-1 panel with
// the house shadow/radius/border, the modal enter animation,
// role="dialog"/aria-modal on the panel (the panel is what a screen reader
// should enter, not the full-screen wrapper), Escape-to-dismiss, and moving
// focus into the panel on open / back to the opener on close.
//
// What each consumer still owns: sizing and inner layout (max-w-*, max-h-*,
// padding, flex/grid, overflow) via `className`, its own accessible name, and
// everything inside the panel.
//
// Deliberately NOT here: a Tab trap. useDialogDismiss's note said fixing Tab
// properly needs one shared primitive for every overlay — this is that
// primitive, so the follow-up is now a small one — but adding a trap is a
// behavior change and this pass is a presentation-layer unification. The
// orchestrator's `.modal-overlay` dialogs and the three portalled full-bleed
// "sheet" dialogs (PromptLibrary, DownloadChatDialog, SavePromptDialog) are
// deliberately not on this base either; see the notes on those files.

// Mounted shells, in mount order (outermost first). Escape dismisses exactly
// ONE layer — the last one mounted — because a dialog summoned from inside
// another (the project settings dialog's confirms, the memories modal's "move
// to team learnings" picker) must not take its parent down with it. Every
// shell listens on `document`, and stopPropagation does not stop a sibling
// listener on the same node, so the "am I on top?" test has to be explicit.
//
// Mount order rather than DOM order: the promote-memory dialog is rendered
// *before* the memories modal it is summoned from and paints above it on
// z-index alone, so document order would answer the wrong dialog. Mount order
// rather than z-index: two dialogs on the same layer stack by which opened
// last, which is what the reader means by "the one on top".
const openShells: unknown[] = [];

export type DialogShellProps = {
  // Accessible name. Pass `label` for a titled dialog, or `labelledBy` when
  // the dialog's own copy is the name (the confirm shape whose body IS the
  // question).
  label?: string;
  labelledBy?: string;
  // Accessible name for the scrim's dismiss affordance, e.g. "Close projects".
  scrimLabel: string;
  onDismiss: () => void;
  // "stacked" paints above ordinary modal peers — for a dialog summoned from
  // inside another one, where equal z-index leaves DOM order to decide.
  layer?: "modal" | "stacked";
  // "top" hangs the panel below the viewport top (the shortcuts overlay)
  // instead of centring it.
  align?: "center" | "top";
  // Panel sizing and inner layout only — the surface is not negotiable.
  className?: string;
  // Where focus lands on open. Defaults to the panel itself.
  initialFocusRef?: RefObject<HTMLElement | null>;
  // data-testid on the PANEL (the thing tests assert about).
  testId?: string;
  children: ReactNode;
};

export function DialogShell({
  label,
  labelledBy,
  scrimLabel,
  onDismiss,
  layer = "modal",
  align = "center",
  className,
  initialFocusRef,
  testId,
  children,
}: DialogShellProps) {
  const panelRef = useRef<HTMLDivElement | null>(null);

  // Register/unregister in the open-shell stack for the Escape ordering above.
  useEffect(() => {
    openShells.push(panelRef);
    return () => {
      const at = openShells.indexOf(panelRef);
      if (at !== -1) openShells.splice(at, 1);
    };
  }, []);

  // onDismiss through a ref so `dismiss` stays stable: useDialogDismiss
  // re-arms its listener whenever its callback changes, and a parent
  // re-render hands us a fresh closure on every keystroke.
  const onDismissRef = useRef(onDismiss);
  useEffect(() => {
    onDismissRef.current = onDismiss;
  });
  const dismiss = useCallback(() => {
    if (openShells[openShells.length - 1] !== panelRef) return;
    onDismissRef.current();
  }, []);
  useDialogDismiss(true, dismiss);

  // Focus moves into the dialog on open and back to whatever opened it on
  // close, so a keyboard user resumes where they were instead of at the top of
  // the page behind. A trigger that the action removed simply cannot take it
  // back — the browser falls back to the document, which is where focus used
  // to sit for the whole life of these dialogs anyway.
  useEffect(() => {
    const opener = (
      typeof document !== "undefined" ? document.activeElement : null
    ) as HTMLElement | null;
    (initialFocusRef?.current ?? panelRef.current)?.focus();
    return () => opener?.focus?.();
  }, [initialFocusRef]);

  return (
    <div
      className={[
        "fixed inset-0 flex justify-center px-4",
        layer === "stacked" ? "z-[60]" : "z-50",
        align === "top" ? "items-start pt-[10vh]" : "items-center",
      ].join(" ")}
    >
      <button
        aria-label={scrimLabel}
        className="absolute inset-0 bg-[var(--color-overlay-strong)] backdrop-blur-[2px]"
        type="button"
        onClick={onDismiss}
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-label={label}
        aria-labelledby={labelledBy}
        data-testid={testId}
        className={[
          // The opaque surface is the point of this component. --color-surface-1
          // is a solid value in both themes (#241b31 / #ffffff), so nothing
          // behind the panel can compete with what is inside it.
          "motion-safe:animate-pop-up-base relative z-10 w-full rounded-[1.25rem] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] shadow-[var(--shadow-md)] outline-none",
          className ?? "",
        ]
          .join(" ")
          .trim()}
      >
        {children}
      </div>
    </div>
  );
}

export default DialogShell;
