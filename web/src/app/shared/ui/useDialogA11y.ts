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

const FOCUSABLE_SELECTOR =
  'a[href], button:not([disabled]), textarea:not([disabled]), input:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])';

export function useDialogA11y(
  open: boolean,
  containerRef: RefObject<HTMLElement | null>,
  onClose: () => void,
): void {
  const onCloseRef = useRef(onClose);
  useEffect(() => {
    onCloseRef.current = onClose;
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

    // Move focus into the dialog: first focusable control, else the container.
    const nodes = focusables();
    (nodes[0] ?? container)?.focus();

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
