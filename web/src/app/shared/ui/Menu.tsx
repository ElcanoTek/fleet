"use client";

// Menu — the shared anchored popover surface for the unified rail (#169). The
// conversation-row kebab, the footer account menu, and the bulk-action pickers
// all render through this one primitive so they read as the same component
// family (same surface, same keyboard contract), per the design.
//
// Optional side flyout (#169 audit / handoff): a Menu may host ONE flyout (e.g.
// "Add to folder" / "Labels") that opens BESIDE the main menu — both visible at
// once — anchored to the triggering menu item and placed to the side, flipping
// to stay on-screen (the handoff's placeFlyout behavior). The caller owns which
// flyout is open (so opening one closes the other); the Menu owns positioning,
// focus, and dismissal.
//
// Accessibility contract:
//   - role="menu" with a focus trap while open; Tab/Shift+Tab cycle within the
//     focused surface; ArrowUp/Down (+ Home/End) move between its items.
//   - Escape on the flyout closes the flyout and returns focus to its trigger
//     item; Escape on the main menu closes the menu and returns focus to the
//     anchor that opened it.
//   - An outside pointer-down (outside BOTH menu and flyout), a scroll, or a
//     resize closes the menu.
//   - Opening focuses the first item; opening the flyout focuses its first item.
//   - The open animation is suppressed under prefers-reduced-motion.
//
// Positioning is fixed and applied imperatively in a layout effect that runs on
// every render while open (the element renders visibility:hidden in JSX, so each
// parent re-render re-applies that hidden style; re-positioning each commit
// re-reveals it before paint — an open menu never blinks out on an unrelated
// state update). The surfaces are portaled to <body> so `fixed` is truly
// viewport-relative (the rail <aside>'s transform/backdrop-filter would
// otherwise be the containing block and fling them off-screen).

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
  // The surfaces only ever render visible controls, so a plain selector is
  // sufficient — and avoids an offsetParent visibility check that has no layout
  // to read in jsdom tests.
  return Array.from(
    container.querySelectorAll<HTMLElement>(
      'button:not([disabled]), input:not([disabled]), [href], [tabindex]:not([tabindex="-1"])',
    ),
  );
}

// handleMenuKeys provides the shared keyboard contract for a popover surface:
// Escape (delegated to onEscape), arrow/Home/End navigation, and a Tab focus
// trap, all scoped to the given container's focusable items.
function handleMenuKeys(
  e: React.KeyboardEvent<HTMLDivElement>,
  container: HTMLElement | null,
  onEscape: () => void,
) {
  if (!container) return;
  if (e.key === "Escape") {
    e.preventDefault();
    e.stopPropagation();
    onEscape();
    return;
  }
  const items = focusableItems(container);
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
    e.preventDefault();
    const dir = e.shiftKey ? -1 : 1;
    items[(index + dir + items.length) % items.length]?.focus();
  }
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

// positionFlyout places a flyout beside its trigger item (the handoff's
// placeFlyout): open to the right, flip to the left if it would overflow, and
// clamp vertically so it stays fully on-screen.
function positionFlyout(flyout: HTMLElement, anchor: DOMRect) {
  const w = flyout.offsetWidth;
  const h = flyout.offsetHeight;
  const vw = window.innerWidth;
  const vh = window.innerHeight;
  let left = anchor.right + 6;
  if (left + w > vw - VIEWPORT_MARGIN) left = anchor.left - w - 6;
  if (left < VIEWPORT_MARGIN) left = VIEWPORT_MARGIN;
  let top = anchor.top - 6;
  if (top + h > vh - VIEWPORT_MARGIN) top = vh - VIEWPORT_MARGIN - h;
  if (top < VIEWPORT_MARGIN) top = VIEWPORT_MARGIN;
  flyout.style.position = "fixed";
  flyout.style.left = `${Math.round(left)}px`;
  flyout.style.top = `${Math.round(top)}px`;
  flyout.style.visibility = "visible";
}

const SURFACE_CLASS =
  "grid min-w-[9rem] gap-0.5 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-2)] p-1.5 shadow-[var(--shadow-md)] outline-none motion-safe:animate-[menu-in_140ms_ease]";

export function Menu({
  open,
  onClose,
  anchorRef,
  children,
  placement = "bottom-end",
  label,
  className,
  flyout,
  flyoutOpen = false,
  flyoutAnchorRef,
  onFlyoutClose,
  flyoutLabel,
}: {
  open: boolean;
  onClose: () => void;
  anchorRef: RefObject<HTMLElement | null>;
  children: ReactNode;
  placement?: MenuPlacement;
  label?: string;
  className?: string;
  // Optional side flyout (e.g. folder/labels). When flyoutOpen, `flyout` renders
  // beside the menu, anchored to flyoutAnchorRef (the triggering item).
  flyout?: ReactNode;
  flyoutOpen?: boolean;
  flyoutAnchorRef?: RefObject<HTMLElement | null>;
  onFlyoutClose?: () => void;
  flyoutLabel?: string;
}) {
  const menuRef = useRef<HTMLDivElement | null>(null);
  const flyoutRef = useRef<HTMLDivElement | null>(null);
  const showFlyout = Boolean(flyout) && flyoutOpen;

  // Measure + reveal before paint. Runs after every render (see file header).
  useLayoutEffect(() => {
    if (!open) return;
    const anchor = anchorRef.current;
    const menu = menuRef.current;
    if (anchor && menu) positionMenu(menu, anchor.getBoundingClientRect(), placement);
    const flyEl = flyoutRef.current;
    const flyAnchor = flyoutAnchorRef?.current;
    if (showFlyout && flyEl && flyAnchor) positionFlyout(flyEl, flyAnchor.getBoundingClientRect());
  });

  // Focus the first item on open.
  useEffect(() => {
    if (!open) return;
    const menu = menuRef.current;
    if (menu) (focusableItems(menu)[0] ?? menu).focus();
  }, [open]);

  // Focus the flyout's first item when it opens.
  useEffect(() => {
    if (!showFlyout) return;
    const flyEl = flyoutRef.current;
    if (flyEl) (focusableItems(flyEl)[0] ?? flyEl).focus();
  }, [showFlyout]);

  // Outside pointer-down (outside BOTH surfaces + the anchor) / scroll / resize
  // close the menu.
  useEffect(() => {
    if (!open) return;
    const onPointerDown = (e: PointerEvent) => {
      const target = e.target as Node;
      if (menuRef.current?.contains(target)) return;
      if (flyoutRef.current?.contains(target)) return;
      if (anchorRef.current?.contains(target)) return;
      onClose();
    };
    const onScrollOrResize = () => onClose();
    document.addEventListener("pointerdown", onPointerDown, true);
    window.addEventListener("resize", onScrollOrResize);
    window.addEventListener("scroll", onScrollOrResize, true);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown, true);
      window.removeEventListener("resize", onScrollOrResize);
      window.removeEventListener("scroll", onScrollOrResize, true);
    };
  }, [open, onClose, anchorRef]);

  const onMenuKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLDivElement>) => {
      handleMenuKeys(e, menuRef.current, () => {
        anchorRef.current?.focus(); // return focus synchronously before unmount
        onClose();
      });
    },
    [onClose, anchorRef],
  );

  const onFlyoutKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLDivElement>) => {
      handleMenuKeys(e, flyoutRef.current, () => {
        flyoutAnchorRef?.current?.focus(); // return focus to the trigger item
        onFlyoutClose?.();
      });
    },
    [flyoutAnchorRef, onFlyoutClose],
  );

  if (!open || typeof document === "undefined") return null;

  return createPortal(
    <>
      <div
        ref={menuRef}
        role="menu"
        aria-label={label}
        tabIndex={-1}
        onKeyDown={onMenuKeyDown}
        style={{ position: "fixed", visibility: "hidden" }}
        className={["z-[400]", SURFACE_CLASS, className ?? ""].join(" ")}
      >
        {children}
      </div>
      {showFlyout ? (
        <div
          ref={flyoutRef}
          role="menu"
          aria-label={flyoutLabel}
          tabIndex={-1}
          onKeyDown={onFlyoutKeyDown}
          style={{ position: "fixed", visibility: "hidden" }}
          className={["z-[410]", SURFACE_CLASS].join(" ")}
        >
          {flyout}
        </div>
      ) : null}
    </>,
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
