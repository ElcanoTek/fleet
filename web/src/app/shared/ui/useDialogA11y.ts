"use client";

import { useEffect, useRef } from "react";
import type { RefObject } from "react";

// useDialogA11y — shared modal accessibility behavior for the Operations Center
// modals (New Task, Settings). When `open` flips true it remembers the element
// that had focus (the trigger), moves focus into the dialog, traps Tab within
// it, closes on Escape, and restores focus to the trigger on close/unmount.
// It adds no animation of its own, so prefers-reduced-motion is honored by
// construction. Kept dependency-free (no focus-trap package) and small enough to
// share across the two modals this PR touches.
//
// onClose is read through a ref so the trap effect depends only on `open`: a
// parent re-render that hands us a fresh onClose closure must NOT tear down and
// re-arm the trap (that would yank focus back to the first field mid-typing).
//
// Note for callers with a close guard (dirty-form confirm): the hook always
// CALLS onClose on Escape — it never closes anything itself — so the guard
// lives in the onClose you pass (decide there whether to actually close).

const FOCUSABLE_SELECTOR =
  'a[href], button:not([disabled]), textarea:not([disabled]), input:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])';

export type DialogA11yOptions = {
  // Where focus lands when the dialog opens. Defaults to the first focusable
  // element (historical behavior); the New Task modal points this at its
  // Prompt textarea so opening the form starts at the field that matters.
  initialFocusRef?: RefObject<HTMLElement | null>;
};

export function useDialogA11y(
  open: boolean,
  containerRef: RefObject<HTMLElement | null>,
  onClose: () => void,
  options?: DialogA11yOptions,
): void {
  const onCloseRef = useRef(onClose);
  useEffect(() => {
    onCloseRef.current = onClose;
  });
  // Same ref pattern as onClose: a fresh options object per render must not
  // re-arm the trap.
  const optionsRef = useRef(options);
  useEffect(() => {
    optionsRef.current = options;
  });

  useEffect(() => {
    if (!open) return;
    const container = containerRef.current;
    // The trigger we restore focus to when the dialog closes.
    const trigger = (typeof document !== "undefined" ? document.activeElement : null) as
      | HTMLElement
      | null;

    const focusables = (): HTMLElement[] =>
      container
        ? Array.from(container.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR)).filter(
            (node) => node.offsetParent !== null || node === document.activeElement,
          )
        : [];

    // Move focus into the dialog: the caller's initial-focus target when set
    // and focusable, else the first focusable control, else the container.
    const initial = optionsRef.current?.initialFocusRef?.current;
    const nodes = focusables();
    (initial ?? nodes[0] ?? container)?.focus();

    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.preventDefault();
        onCloseRef.current();
        return;
      }
      if (event.key !== "Tab" || !container) return;
      const trapped = focusables();
      if (trapped.length === 0) {
        event.preventDefault();
        container.focus();
        return;
      }
      const first = trapped[0];
      const last = trapped[trapped.length - 1];
      const active = document.activeElement;
      const inside = container.contains(active);
      if (event.shiftKey && (active === first || active === container || !inside)) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && (active === last || !inside)) {
        event.preventDefault();
        first.focus();
      }
    };

    document.addEventListener("keydown", onKeyDown, true);
    return () => {
      document.removeEventListener("keydown", onKeyDown, true);
      if (trigger && typeof trigger.focus === "function") trigger.focus();
    };
  }, [open, containerRef]);
}

export default useDialogA11y;
