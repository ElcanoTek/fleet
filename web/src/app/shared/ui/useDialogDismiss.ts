"use client";

// Escape closes a dialog. One hook, because five new dialogs shipped without
// it and every one of them trapped a keyboard user's most reflexive way out.
//
// This is NOT a focus trap — Tab still walks out of these dialogs into the
// page behind them, matching the pre-existing house pattern (ProjectsModal,
// the memories modal). Fixing that properly means one shared dialog primitive
// for every overlay in the app, which is a wider change than this pass; what
// is here is the part that is cheap, correct, and independently useful.

import { useEffect } from "react";

export function useDialogDismiss(open: boolean, onDismiss: () => void) {
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      // Let a nested control (a select's dropdown, an inline confirm) answer
      // Escape first — they stop propagation when they consume it.
      e.stopPropagation();
      onDismiss();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [open, onDismiss]);
}
