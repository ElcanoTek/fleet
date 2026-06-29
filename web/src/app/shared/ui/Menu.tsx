"use client";

// Menu — the shared anchored popover surface for the unified rail (#169). Both
// the conversation-row kebab and the footer account menu render through this one
// primitive so the two read as the same component family (same surface, same
// keyboard contract), per the design.
//
// Accessibility contract:
//   - role="menu" with a focus trap while open; Tab/Shift+Tab cycle within.
//   - ArrowDown/ArrowUp (+ Home/End) move between focusable items.
//   - Escape closes and returns focus to the anchor that opened it.
//   - An outside pointer-down, a scroll, or a resize closes it (the popover is
//     position:fixed, so it would otherwise detach from a scrolled anchor).
//   - Opening focuses the first focusable item; closing restores focus.
//   - The open animation is suppressed under prefers-reduced-motion.
//
// Positioning is fixed (escapes the rail's overflow without a portal) and is
// applied imperatively in a layout effect — the popover renders hidden, then is
// measured against its anchor and revealed before paint, so there is no flash
// and no position-in-state cascade. Two placements cover the rail's needs:
// "bottom-end" (kebab → below, right-aligned) and "top-stretch" (account button
// → above, matching the anchor's width).

import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  type ReactNode,
  type RefObject,
} from "react";
import { createPortal } from "react-dom";

export type MenuPlacement = "bottom-end" | "top-stretch";

const VIEWPORT_MARGIN = 8;

function focusableItems(container: HTMLElement): HTMLElement[] {
  // The menu only ever renders visible controls (panels swap content rather
  // than hiding it), so a plain selector is sufficient — and avoids an
  // offsetParent visibility check that has no layout to read in jsdom tests.
  return Array.from(
    container.querySelectorAll<HTMLElement>(
      'button:not([disabled]), input:not([disabled]), [href], [tabindex]:not([tabindex="-1"])',
    ),
  );
}

function positionMenu(menu: HTMLElement, anchor: DOMRect, placement: MenuPlacement) {
  // The menu is portaled to <body>, so these are true viewport coordinates.
  // offsetWidth/offsetHeight are measurable while visibility:hidden (still laid out).
  const w = menu.offsetWidth;
  const h = menu.offsetHeight;
  const vw = window.innerWidth;
  const vh = window.innerHeight;
  const clamp = (v: number, max: number) => Math.min(Math.max(v, VIEWPORT_MARGIN), Math.max(VIEWPORT_MARGIN, max));

  menu.style.position = "fixed";
  if (placement === "top-stretch") {
    menu.style.right = "auto";
    menu.style.top = "auto";
    menu.style.left = `${Math.round(anchor.left)}px`;
    menu.style.width = `${Math.round(anchor.width)}px`;
    menu.style.bottom = `${Math.round(vh - anchor.top + 6)}px`;
  } else {
    // bottom-end: right edges aligned to the anchor, but clamped fully on-screen
    // (a rail kebab sits near the left edge, so a right-aligned menu would
    // otherwise overflow off the left). Vertically: below the anchor, flipping
    // above it when there is no room below.
    const left = clamp(anchor.right - w, vw - w - VIEWPORT_MARGIN);
    let top = anchor.bottom + 4;
    if (top + h > vh - VIEWPORT_MARGIN) {
      const above = anchor.top - 4 - h;
      top = above >= VIEWPORT_MARGIN ? above : clamp(vh - h - VIEWPORT_MARGIN, vh - h - VIEWPORT_MARGIN);
    }
    menu.style.right = "auto";
    menu.style.bottom = "auto";
    menu.style.left = `${Math.round(left)}px`;
    menu.style.top = `${Math.round(top)}px`;
  }
  menu.style.visibility = "visible";
}

export function Menu({
  open,
  onClose,
  anchorRef,
  children,
  placement = "bottom-end",
  label,
  className,
}: {
  open: boolean;
  onClose: () => void;
  anchorRef: RefObject<HTMLElement | null>;
  children: ReactNode;
  placement?: MenuPlacement;
  label?: string;
  className?: string;
}) {
  const menuRef = useRef<HTMLDivElement | null>(null);

  // Measure against the anchor and reveal before paint — no flash, no state.
  // Intentionally runs after EVERY render (no dep array): the element renders
  // with visibility:hidden in JSX, so each re-render of a parent (e.g. the chat
  // conversation-list poll) re-applies that hidden style; re-positioning on
  // every commit re-reveals it before paint, so an open menu never blinks out
  // when unrelated state updates. Cheap — it early-returns while closed.
  useLayoutEffect(() => {
    if (!open) return;
    const anchor = anchorRef.current;
    const menu = menuRef.current;
    if (!anchor || !menu) return;
    positionMenu(menu, anchor.getBoundingClientRect(), placement);
  });

  // Focus the first item on open. (Focus return is handled synchronously in the
  // Escape handler — by the time a passive cleanup runs the menu has already
  // unmounted and focus has left it, so a cleanup-based restore is unreliable.)
  useEffect(() => {
    if (!open) return;
    const menu = menuRef.current;
    if (menu) {
      const first = focusableItems(menu)[0];
      (first ?? menu).focus();
    }
  }, [open]);

  // Outside pointer-down / scroll / resize close the menu.
  useEffect(() => {
    if (!open) return;
    const onPointerDown = (e: PointerEvent) => {
      const target = e.target as Node;
      if (menuRef.current?.contains(target)) return;
      if (anchorRef.current?.contains(target)) return;
      onClose();
    };
    const onScrollOrResize = () => onClose();
    document.addEventListener("pointerdown", onPointerDown, true);
    window.addEventListener("resize", onScrollOrResize);
    // capture: catch scrolls on any ancestor scroll container, not just window.
    window.addEventListener("scroll", onScrollOrResize, true);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown, true);
      window.removeEventListener("resize", onScrollOrResize);
      window.removeEventListener("scroll", onScrollOrResize, true);
    };
  }, [open, onClose, anchorRef]);

  const onKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLDivElement>) => {
      const menu = menuRef.current;
      if (!menu) return;
      if (e.key === "Escape") {
        e.preventDefault();
        e.stopPropagation();
        // Return focus to the anchor synchronously, before the menu unmounts.
        anchorRef.current?.focus();
        onClose();
        return;
      }
      const items = focusableItems(menu);
      if (items.length === 0) return;
      const active = document.activeElement as HTMLElement | null;
      const index = active ? items.indexOf(active) : -1;
      if (e.key === "ArrowDown") {
        e.preventDefault();
        items[(index + 1 + items.length) % items.length]?.focus();
      } else if (e.key === "ArrowUp") {
        e.preventDefault();
        items[(index - 1 + items.length) % items.length]?.focus();
      } else if (e.key === "Home") {
        e.preventDefault();
        items[0]?.focus();
      } else if (e.key === "End") {
        e.preventDefault();
        items[items.length - 1]?.focus();
      } else if (e.key === "Tab") {
        // Trap focus within the menu.
        e.preventDefault();
        const dir = e.shiftKey ? -1 : 1;
        items[(index + dir + items.length) % items.length]?.focus();
      }
    },
    [onClose, anchorRef],
  );

  if (!open || typeof document === "undefined") return null;

  // Portal to <body>: the rail <aside> sets a transform (mobile drawer slide)
  // and a backdrop-filter, either of which makes it the containing block for
  // position:fixed descendants — which would resolve the menu's viewport
  // coordinates against the 288px rail instead, flinging it off-screen. Rendering
  // at the document root keeps `fixed` truly viewport-relative.
  return createPortal(
    <div
      ref={menuRef}
      role="menu"
      aria-label={label}
      tabIndex={-1}
      onKeyDown={onKeyDown}
      // Hidden until the layout effect measures + reveals it before paint.
      style={{ position: "fixed", visibility: "hidden" }}
      className={[
        "z-[400] grid min-w-[9rem] gap-0.5 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-2)] p-1.5 shadow-[var(--shadow-md)] outline-none",
        "motion-safe:animate-[menu-in_140ms_ease]",
        className ?? "",
      ].join(" ")}
    >
      {children}
    </div>,
    document.body,
  );
}

// MenuItem — a single actionable row (role="menuitem"). `icon` is a core-icons
// sprite name; `trailing` renders at the row's end (caret, check, badge).
export function MenuItem({
  icon,
  children,
  onClick,
  danger,
  disabled,
  trailing,
  ariaHasPopup,
  ariaExpanded,
}: {
  icon?: ReactNode;
  children: ReactNode;
  onClick?: (e: React.MouseEvent<HTMLButtonElement>) => void;
  danger?: boolean;
  disabled?: boolean;
  trailing?: ReactNode;
  ariaHasPopup?: boolean;
  ariaExpanded?: boolean;
}) {
  return (
    <button
      type="button"
      role="menuitem"
      disabled={disabled}
      aria-haspopup={ariaHasPopup}
      aria-expanded={ariaExpanded}
      onClick={onClick}
      className={[
        "flex w-full items-center gap-2 rounded-[0.4rem] px-2 py-1.5 text-left text-[0.8125rem] transition",
        "focus-visible:bg-[var(--color-overlay-soft)] focus-visible:outline-none",
        disabled ? "cursor-not-allowed opacity-50" : "",
        danger
          ? "text-[var(--color-danger)] hover:bg-[color-mix(in_srgb,var(--color-danger)_14%,transparent)]"
          : "text-[var(--color-text-secondary)] hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]",
      ].join(" ")}
    >
      {icon ? <span className="grid size-4 shrink-0 place-items-center text-current">{icon}</span> : null}
      <span className="min-w-0 flex-1 truncate">{children}</span>
      {trailing ? <span className="ml-auto shrink-0">{trailing}</span> : null}
    </button>
  );
}

export function MenuSeparator() {
  return <div role="separator" className="my-1 h-px bg-[var(--color-border)]" />;
}

export function MenuSectionLabel({ children }: { children: ReactNode }) {
  return (
    <div className="px-2 pb-1 pt-1 text-[0.625rem] font-medium uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
      {children}
    </div>
  );
}

export default Menu;
