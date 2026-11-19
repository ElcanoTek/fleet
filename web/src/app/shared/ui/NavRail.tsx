import { t as tr } from "@/app/lib/i18n";
// NavRail — the one sidebar every signed-in page renders (docs/SIDEBAR.md):
// Chat, Patrols, Background tasks, Settings, Guides and Analytics. It owns the
// frame: the brand row (with the gear that opens Customize and the collapse
// toggle), a slot for the search / new-chat / + row, ONE scrolling body that
// holds the top items (sidebar/RailNav.tsx) and whatever the page passes as
// children (the Pinned, Projects and Chats lists), a footer slot, and the
// account menu. Nothing inside the body scrolls on its own: the sidebar
// scrolls as a whole.
//
// Responsive/collapse model:
//   - <640px (below sm): an off-canvas drawer, toggled via sidebarOpen with a
//     tap-to-dismiss backdrop and the hamburger in PageTopBar. The drawer
//     always shows the full sidebar regardless of collapse state.
//   - ≥sm: an in-flow sticky column. Expanded it is as wide as the person
//     dragged it (300px until they do); collapsed it is a 3.75rem icon strip
//     (2.5rem squares with data-tip labels, avatar-only account menu). The
//     collapse toggle lives in the brand row; collapsed, the brand mark is
//     the button that expands it. Both preferences persist to localStorage
//     ("rail-collapsed", "rail-width"): they are about this screen, not the
//     account. Width/padding animate on the fast motion token, gated behind
//     `railReady` so first paint never animates, and never while dragging.
//   - 640–900px (`isNarrow`): the rail auto-collapses (state only — the stored
//     preference is untouched). Expanding it there opens the rail as a fixed
//     overlay over the content with a scrim (click or Escape dismisses, focus
//     returns to the toggle, Tab is trapped inside) instead of pushing the
//     layout.
//
// The owning shells call useRailCollapse() and pass the result in, so the
// page's content can adapt to the same state without a second source of truth.

import { useCallback, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { RailNav } from "@/app/shared/sidebar/RailNav";
import { SidebarCustomizer } from "@/app/shared/sidebar/SidebarCustomizer";
import { CustomizeButton } from "@/app/shared/sidebar/CustomizePopover";
import { PatrolWizardHost } from "@/app/launchpad/patrolWizard";
import { useSidebar } from "@/app/shared/sidebar/store";
import { CloseButton } from "./CloseButton";
import { Icon } from "./Icon";
import { AccountMenu } from "./AccountMenu";
import { isInteractiveTarget } from "./interactiveTarget";
import { RAIL_ICON_BUTTON_CLASS } from "./railStyles";
import { cn } from "@/lib/utils";
import { readPref, writePref } from "@/app/shared/lib/storage";

// Which destination the page is. "patrols" and "orchestrator" light their
// own top item; "settings", "analytics" and "help" are reached from the
// account menu, so they tint the account button instead.
export type RailView = "chat" | "patrols" | "orchestrator" | "settings" | "analytics" | "help";

export const RAIL_COLLAPSED_STORAGE_KEY = "rail-collapsed";
export const RAIL_WIDTH_STORAGE_KEY = "rail-width";
const NARROW_QUERY = "(max-width: 900px)";

// The width a person can drag the sidebar to, in CSS pixels. The floor keeps
// the search field and its two buttons on one row; the ceiling keeps the page
// the larger part of the window (the stylesheet also caps it at 45vw).
export const RAIL_MIN_WIDTH = 240;
export const RAIL_MAX_WIDTH = 480;
export const RAIL_DEFAULT_WIDTH = 300;
const RAIL_KEY_STEP = 16;

export function clampRailWidth(px: number): number {
  return Math.round(Math.min(RAIL_MAX_WIDTH, Math.max(RAIL_MIN_WIDTH, px)));
}

export type RailCollapse = {
  /** Icon-strip mode (≥sm). Auto-true at ≤900px; user-set persists. */
  collapsed: boolean;
  /** Viewport ≤900px — expanding the rail overlays the content with a scrim. */
  isNarrow: boolean;
  /** Gates the width/padding transition so first paint never animates. */
  railReady: boolean;
  setCollapsed: (next: boolean) => void;
  /** The width the person dragged the sidebar to; null until they do. */
  width?: number | null;
  /** Sets (and remembers) the width; null goes back to the default. */
  setWidth?: (next: number | null) => void;
};

function readStoredWidth(): number | null {
  const raw = Number(readPref(RAIL_WIDTH_STORAGE_KEY));
  return Number.isFinite(raw) && raw > 0 ? clampRailWidth(raw) : null;
}

// useRailCollapse owns the collapse state and the width for a shell. All
// reads happen in the mount effect (never in the initializer): the state
// starts expanded at the default width and reconciles to the persisted
// preferences before `railReady` enables the transition, so there is no
// first-paint animation.
export function useRailCollapse(): RailCollapse {
  const [collapsed, setCollapsedState] = useState(false);
  const [isNarrow, setIsNarrow] = useState(false);
  const [railReady, setRailReady] = useState(false);
  const [width, setWidthState] = useState<number | null>(null);

  useEffect(() => {
    const mql = window.matchMedia(NARROW_QUERY);
    const syncFromEnvironment = () => {
      // Private-mode / storage-disabled: default expanded.
      const stored = readPref(RAIL_COLLAPSED_STORAGE_KEY) === "1";
      setIsNarrow(mql.matches);
      // Narrow viewports auto-collapse without touching the stored preference.
      setCollapsedState(stored || mql.matches);
      setWidthState(readStoredWidth());
    };
    syncFromEnvironment();
    const onChange = () => {
      setIsNarrow(mql.matches);
      if (mql.matches) setCollapsedState(true);
    };
    mql.addEventListener("change", onChange);
    // Double-rAF: enable the width transition only after the reconciled state
    // has painted (the design's .rail.ready gate).
    const raf = requestAnimationFrame(() => requestAnimationFrame(() => setRailReady(true)));
    return () => {
      mql.removeEventListener("change", onChange);
      cancelAnimationFrame(raf);
    };
  }, []);

  const setCollapsed = useCallback((next: boolean) => {
    setCollapsedState(next);
    // Private-mode / storage-disabled: state stays session-only.
    writePref(RAIL_COLLAPSED_STORAGE_KEY, next ? "1" : "0");
  }, []);

  const setWidth = useCallback((next: number | null) => {
    const value = next === null ? null : clampRailWidth(next);
    setWidthState(value);
    writePref(RAIL_WIDTH_STORAGE_KEY, value === null ? "" : String(value));
  }, []);

  return { collapsed, isNarrow, railReady, setCollapsed, width, setWidth };
}

// Focusable elements inside the overlay drawer, for the Tab trap. Mirrors the
// selector Menu.tsx uses. HTML elements only: `[href]` also matches the
// <use href> inside every icon, which cannot take focus (and reports no
// offsetParent at all, so the visibility test let it through as the "first"
// item).
function overlayFocusables(container: HTMLElement): HTMLElement[] {
  return Array.from(
    container.querySelectorAll<HTMLElement>(
      'button:not([disabled]), input:not([disabled]), [href], [tabindex]:not([tabindex="-1"])',
    ),
  ).filter((el) => el instanceof HTMLElement && el.offsetParent !== null);
}

// Chat's "j" / "k" shortcut opens the phone drawer to show the list cursor,
// and its Enter opens the chat under that cursor, but only while no control
// has focus. The shortcut calls this just before it opens the drawer (not
// when the drawer is already open), so the drawer can tell that opening from
// one made with its button. The drawer's effect reads the mark and clears it
// every time the drawer opens or closes, so a mark nothing used (the rail is
// not a drawer at this width) is gone by the close that must come before the
// next opening. No clock is involved: an earlier version let the mark expire
// after half a second, which made a slow render look like a button press.
const LAYER_ABOVE_DRAWER = '[role="menu"], [role="dialog"], [role="alertdialog"], [role="listbox"]';
let drawerOpenedByShortcut = false;
export function keepFocusForDrawerShortcut(): void {
  drawerOpenedByShortcut = true;
}

export function NavRail({
  activeView,
  brandName,
  // fleet's own mark is the fallback, NOT any one client's: a deployment brands
  // the rail by declaring branding.logo in its client-config bundle, which
  // arrives here as the proxied /api/brand/logo path. Callers pass
  // `branding.logo_url || undefined` so an unset bundle falls back here.
  brandLogoSrc = "/logos/fleet-mark.svg",
  opsCount,
  sidebarOpen,
  setSidebarOpen,
  collapse,
  account,
  actions,
  footer,
  children,
}: {
  activeView: RailView;
  brandName: string;
  brandLogoSrc?: string;
  opsCount?: number;
  sidebarOpen: boolean;
  setSidebarOpen: (open: boolean) => void;
  collapse: RailCollapse;
  account: { email: string; onSignOut: () => void };
  // The row under the brand: search, new chat and the + menu. It stays put
  // while the body below it scrolls.
  actions?: ReactNode;
  // Pinned under the body, above the account menu (the select-mode bar).
  footer?: ReactNode;
  // The page's lists (Pinned, Projects, Chats), inside the one scroller.
  children?: ReactNode;
}) {
  const { layout } = useSidebar();
  const compact = Boolean(layout.compact);
  const { collapsed, isNarrow, railReady, setCollapsed } = collapse;
  // Expanded on a narrow (640–900px) viewport → fixed overlay + scrim.
  const overlay = isNarrow && !collapsed;
  const collapseToggleRef = useRef<HTMLButtonElement | null>(null);
  const asideRef = useRef<HTMLElement | null>(null);

  // The closed phone drawer is invisible, so whatever held focus inside it
  // would lose it to <body> and the next Tab would start from the top of the
  // page. Closing it — its own Close button, Escape, choosing a destination —
  // returns focus to the button that opens it. From sm up that button is
  // hidden and the rail never closes, so nothing happens there.
  const wasOpenRef = useRef(sidebarOpen);
  useEffect(() => {
    const wasOpen = wasOpenRef.current;
    wasOpenRef.current = sidebarOpen;
    if (!wasOpen || sidebarOpen) return;
    if (!asideRef.current?.contains(document.activeElement)) return;
    Array.from(document.querySelectorAll<HTMLButtonElement>("button[data-sidebar-toggle]"))
      .find((button) => button.offsetParent !== null)
      ?.focus();
  }, [sidebarOpen]);

  // The phone drawer (below sm) covers the page like the overlay below does,
  // so it gets the same keyboard manners. Without them, on every page but
  // Chat (which closes it on Escape itself), Escape did nothing, focus stayed
  // on the button underneath, and Tab walked the page hidden behind the
  // drawer before it reached the drawer. That is the layout a desktop zoomed
  // far in falls into, where the keyboard is the way around.
  //   - opening moves focus into the drawer;
  //   - Escape closes it (the effect above then hands focus to the toggle);
  //   - Tab stays within the drawer.
  // A menu or dialog opened from the drawer is portaled out of it and runs
  // its own trap, so focus inside one is left alone.
  useEffect(() => {
    const byShortcut = drawerOpenedByShortcut;
    drawerOpenedByShortcut = false;
    if (!sidebarOpen) return;
    // The same test the layout uses (Tailwind's sm is 40rem, which follows
    // the reader's font size), so the handling is on exactly while the rail
    // is a drawer.
    const isPhone = () =>
      typeof window.matchMedia === "function" &&
      window.matchMedia("(width < 40rem)").matches;
    if (!isPhone()) return;
    // On the frame the drawer opens it is still `invisible` (the visibility
    // change rides the slide's transition and has not started), and a hidden
    // element refuses focus: so try now, then again once it has begun.
    // Where focus goes when the drawer opens, by how it was opened and what
    // had focus (NavRail.test.tsx has one test per row):
    //
    //   opened by            focus was on             focus goes
    //   its button, a tap    anything                 into the drawer
    //   the j / k shortcut   a control (owns Enter)   into the drawer
    //   the j / k shortcut   anything else, nothing   stays where it is
    //
    // The last row is the only exception, and it needs both halves. Enter
    // opens the chat under the list cursor only while no control has focus,
    // so the shortcut must not put focus on one. But a control that already
    // has focus is now underneath the drawer, and leaving it there would
    // let Enter press something the reader cannot see. "Is a control" is
    // the very test Enter uses (isInteractiveTarget), so the two cannot
    // disagree. "Was a control focused" cannot replace "was it opened from
    // its button" either: Safari does not focus a button when it is clicked
    // or tapped.
    const stayOnPage = byShortcut && !isInteractiveTarget(document.activeElement);
    const focusIn = () => {
      const aside = asideRef.current;
      if (!stayOnPage && aside && !aside.contains(document.activeElement)) {
        overlayFocusables(aside)[0]?.focus();
      }
    };
    focusIn();
    let frame = requestAnimationFrame(() => {
      focusIn();
      frame = requestAnimationFrame(focusIn);
    });
    const onKey = (e: KeyboardEvent) => {
      if (!isPhone()) return;
      const rail = asideRef.current;
      // A menu or dialog opened from the drawer sits on top of it and is
      // portaled out of it. Its keys are its own: Escape there closes that
      // layer only (closing the drawer too would leave it handing focus back
      // to a button that is no longer on screen), and Tab is its trap's.
      const active = document.activeElement;
      const inLayerAbove =
        active instanceof Element &&
        !(rail?.contains(active) ?? false) &&
        active.closest(LAYER_ABOVE_DRAWER) !== null;
      if (e.key === "Escape") {
        // defaultPrevented: a dialog's own handler (useDialogA11y listens on
        // document, before this) has already used this Escape to close
        // itself and put focus back on its trigger inside the rail, so by
        // now focus alone no longer says a layer was open.
        if (!inLayerAbove && !e.defaultPrevented) setSidebarOpen(false);
        return;
      }
      if (e.key !== "Tab" || inLayerAbove) return;
      if (!rail) return;
      const items = overlayFocusables(rail);
      if (items.length === 0) return;
      const first = items[0];
      const last = items[items.length - 1];
      if (active instanceof Element && !rail.contains(active)) {
        e.preventDefault();
        (e.shiftKey ? last : first).focus();
      } else if (e.shiftKey && active === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && active === last) {
        e.preventDefault();
        first.focus();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => {
      cancelAnimationFrame(frame);
      window.removeEventListener("keydown", onKey);
    };
  }, [sidebarOpen, setSidebarOpen]);

  // Modal-drawer keyboard behavior while the narrow overlay is up, on ONE
  // window listener:
  //   - Escape dismisses the overlay and returns focus to the toggle.
  //   - Tab cycles within the rail's own controls (the Tab trap).
  // Both live here rather than as an onKeyDown on the <aside> below. <aside> is
  // a `complementary` landmark — a non-interactive element — and hanging key
  // handlers on one is what jsx-a11y/no-noninteractive-element-interactions
  // objects to; the honest fix is to move the handler off it, not to paint on a
  // widget role the element does not have. Behavior is unchanged: an open menu
  // stops the event before it reaches window (React's stopPropagation also
  // stops the native event, and React's listener sits on the root container
  // below window), and the trap still only acts when the focused element is the
  // rail's own first/last focusable, which is exactly when the aside-scoped
  // handler used to act. Portaled menus manage their own trap and are not in
  // the aside's DOM subtree, so overlayFocusables never sees them.
  useEffect(() => {
    if (!overlay) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setCollapsed(true);
        collapseToggleRef.current?.focus();
        return;
      }
      if (e.key !== "Tab") return;
      const aside = asideRef.current;
      if (!aside) return;
      const items = overlayFocusables(aside);
      if (items.length === 0) return;
      const first = items[0];
      const last = items[items.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [overlay, setCollapsed]);

  // Dragging the sidebar's edge sets its width. The pointer is captured by
  // the handle, so the drag keeps going over the page (and over an iframe in
  // it); the width follows the pointer from the sidebar's own left edge, so
  // it never drifts from the cursor. Released, the width is remembered.
  const [dragging, setDragging] = useState(false);
  const setWidth = collapse.width !== undefined ? collapse.setWidth : undefined;
  const resizable = Boolean(setWidth) && !collapsed && !overlay;
  const widthNow = () => Math.round(asideRef.current?.getBoundingClientRect().width ?? RAIL_DEFAULT_WIDTH);
  const onResizePointerDown = (e: React.PointerEvent<HTMLDivElement>) => {
    if (!setWidth || e.button !== 0) return;
    e.preventDefault();
    e.currentTarget.setPointerCapture?.(e.pointerId);
    setDragging(true);
  };
  const onResizePointerMove = (e: React.PointerEvent<HTMLDivElement>) => {
    if (!dragging || !setWidth) return;
    const left = asideRef.current?.getBoundingClientRect().left ?? 0;
    setWidth(e.clientX - left);
  };
  const endResize = (e: React.PointerEvent<HTMLDivElement>) => {
    if (!dragging) return;
    e.currentTarget.releasePointerCapture?.(e.pointerId);
    setDragging(false);
  };
  const onResizeKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    if (!setWidth) return;
    if (e.key === "ArrowLeft" || e.key === "ArrowRight") {
      e.preventDefault();
      setWidth(widthNow() + (e.key === "ArrowRight" ? RAIL_KEY_STEP : -RAIL_KEY_STEP));
    } else if (e.key === "Home") {
      e.preventDefault();
      setWidth(RAIL_MIN_WIDTH);
    } else if (e.key === "End") {
      e.preventDefault();
      setWidth(RAIL_MAX_WIDTH);
    } else if (e.key === "Enter") {
      e.preventDefault();
      setWidth(null);
    }
  };

  // The collapsed strip names each icon in a label beside it. The strip's
  // body scrolls, and a scroller clips what is drawn beside it, so the label
  // is placed on the page itself: one for the whole body, shown for whichever
  // [data-tip] item the pointer or the keyboard is on. It is decoration: every
  // item also has a name a screen reader announces.
  const bodyRef = useRef<HTMLDivElement | null>(null);
  const [tip, setTip] = useState<{ text: string; top: number; left: number } | null>(null);
  useEffect(() => {
    const body = bodyRef.current;
    if (!body || !collapsed) return;
    const show = (e: Event) => {
      const el = e.target instanceof Element ? e.target.closest("[data-tip]") : null;
      if (!el || !body.contains(el)) return;
      // Focus shows it for the keyboard only: a click focuses too, and with
      // no pointer to move away the label would stay.
      if (e.type === "focusin" && !el.matches(":focus-visible")) return;
      const r = el.getBoundingClientRect();
      setTip({ text: el.getAttribute("data-tip") ?? "", top: Math.round(r.top + r.height / 2), left: Math.round(r.right + 8) });
    };
    const hide = () => setTip(null);
    body.addEventListener("pointerover", show);
    body.addEventListener("pointerout", hide);
    body.addEventListener("focusin", show);
    body.addEventListener("focusout", hide);
    body.addEventListener("scroll", hide);
    body.addEventListener("click", hide);
    return () => {
      body.removeEventListener("pointerover", show);
      body.removeEventListener("pointerout", hide);
      body.removeEventListener("focusin", show);
      body.removeEventListener("focusout", hide);
      body.removeEventListener("scroll", hide);
      body.removeEventListener("click", hide);
      setTip(null);
    };
  }, [collapsed]);

  // Where a scrollbar takes room (most desktops off macOS), the body's
  // gutter is that room, reserved at its right edge: the body gives the same
  // amount back from its own right padding, so rows sit as far from the
  // sidebar's right edge as from its left, on every platform.
  const [gutter, setGutter] = useState(0);
  useLayoutEffect(() => {
    const body = bodyRef.current;
    if (body) setGutter(Math.max(0, body.offsetWidth - body.clientWidth));
  }, [collapsed, sidebarOpen, railReady]);

  const width = collapse.width ?? null;
  const brandMark = (
    // A plain <img>, never an optimizer: the bundle mark arrives as
    // "/api/brand/logo" (no extension) and may be an SVG. Under next/image
    // that combination rendered a broken image on every page;
    // NavRail.test.tsx keeps the src pointing at the route.
    <img src={brandLogoSrc} alt={brandName} width={28} height={28} fetchPriority="high" decoding="async" className="size-7 shrink-0" style={{ color: "transparent" }} />
  );

  return (
    <>
      {/* Mobile backdrop — taps outside the drawer dismiss it. */}
      <button
        aria-label={tr("Close navigation")}
        className={[
          "fixed inset-0 z-20 bg-[var(--color-overlay-strong)] backdrop-blur-[2px] transition sm:hidden",
          sidebarOpen ? "block" : "hidden",
        ].join(" ")}
        type="button"
        onClick={() => setSidebarOpen(false)}
      />

      {/* Narrow-viewport scrim (the design's .rail-scrim): shown behind the
          expanded overlay rail at 640–900px; click to dismiss. */}
      {overlay ? (
        <button
          aria-label={tr("Close navigation")}
          type="button"
          className="fixed inset-0 z-[380] hidden bg-[var(--color-overlay-strong)] sm:block"
          onClick={() => setCollapsed(true)}
        />
      ) : null}

      <aside
        ref={asideRef}
        aria-label={tr("Primary navigation")}
        data-density={compact ? "compact" : "comfortable"}
        data-collapsed={collapsed ? "true" : undefined}
        className={[
          // <sm: the off-canvas drawer (full rail content, translate toggled).
          // --sidebar-surface is a gradient, so it must land in
          // background-image (the un-hinted arbitrary-value form emits
          // background-color, which drops gradient values); --color-bg
          // underneath keeps the drawer opaque over scrolled content.
          "rail group/rail fixed inset-y-0 left-0 z-30 flex h-[100dvh] w-[min(20rem,88vw)] flex-col border-r border-[var(--color-border)] bg-[var(--color-bg)] bg-[image:var(--sidebar-surface)] pt-[max(0.6rem,env(safe-area-inset-top))] shadow-[var(--shadow-lg)] backdrop-blur-xl transition-[translate,visibility] duration-base max-sm:overflow-hidden",
          // Closed on a phone it is also invisible, not only slid off-screen:
          // an off-screen drawer still took keyboard focus (the first Tab
          // stops on every page were its hidden items) and was still read
          // out. Visibility joins the transition, so it stays painted until
          // the slide-out has finished. (The property that moves it is
          // `translate`, which is what Tailwind's translate-x-* sets; naming
          // `transform` here left the slide with nothing to animate.)
          sidebarOpen ? "translate-x-0" : "-translate-x-full max-sm:invisible",
          // ≥sm: in-flow sticky column (or the fixed narrow overlay). The
          // rail surface is the semi-transparent --sidebar-surface gradient
          // over the app-wide --gradient-bg on <body>.
          "sm:z-30 sm:translate-x-0 sm:bg-transparent sm:backdrop-blur-0",
          overlay
            ? "sm:fixed sm:inset-y-0 sm:left-0 sm:z-[390] sm:shadow-[var(--shadow-md)]"
            : "sm:sticky sm:top-0 sm:h-screen sm:shadow-none",
          // The width the person set (--rail-w), within what the window can
          // spare; 300px until they drag it, growing with their font size.
          collapsed ? "sm:w-[3.75rem]" : "sm:w-[var(--rail-w)]",
          railReady && !dragging ? "sm:transition-[width] sm:duration-fast" : "",
        ].join(" ")}
        style={
          {
            "--rail-w":
              width === null
                ? `clamp(${RAIL_DEFAULT_WIDTH}px, 18.75rem, 34vw)`
                : `min(${width}px, 45vw)`,
            "--rail-row-h": compact ? "1.875rem" : "2.25rem",
            "--rail-row-text": compact ? "0.8125rem" : "0.875rem",
            "--rail-pad-l": "max(0.5rem, env(safe-area-inset-left))",
            "--rail-gutter": `${gutter}px`,
          } as React.CSSProperties
        }
      >
        {/* Brand row — mark + name, then the gear (Customize) and the
            collapse toggle. Collapsed (≥sm) the mark is the button that
            expands the sidebar, with the gear under it. */}
        <div
          className={[
            "flex shrink-0 items-center gap-0.5 pb-2 pl-[calc(var(--rail-pad-l)+0.25rem)] pr-2",
            // justify-between stays on below sm in both states: narrow
            // viewports always report collapsed, and dropping it there parked
            // the drawer's close X against the brand name instead of at the
            // row's end.
            collapsed ? "sm:flex-col sm:gap-1.5 sm:px-0" : "",
          ].join(" ")}
        >
          <div className={["flex min-w-0 flex-1 items-center gap-2.5", collapsed ? "sm:hidden" : ""].join(" ")}>
            {brandMark}
            <span className="min-w-0 truncate font-heading text-[1rem] font-bold text-[var(--color-text-primary)]">
              {brandName}
            </span>
          </div>
          {collapsed ? (
            // The strip has no room for a toggle of its own: the mark opens
            // the sidebar, and says so on hover and to a screen reader.
            <button
              ref={collapseToggleRef}
              type="button"
              aria-label={tr("Expand sidebar")}
              aria-expanded={false}
              data-tip={tr("Expand")}
              data-testid="rail-expand"
              className="group/mark relative hidden size-10 items-center justify-center rounded-[var(--radius-md)] transition hover:bg-[var(--rail-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] sm:inline-flex"
              onClick={() => setCollapsed(false)}
            >
              <span className="transition group-hover/mark:opacity-0 group-focus-visible/mark:opacity-0">{brandMark}</span>
              <Icon
                name="panel-left"
                className="absolute size-[1.05rem] text-[var(--color-text-primary)] opacity-0 transition group-hover/mark:opacity-100 group-focus-visible/mark:opacity-100"
              />
            </button>
          ) : null}
          <CustomizeButton collapsed={collapsed} />
          {collapsed ? null : (
            <button
              ref={collapseToggleRef}
              aria-label={tr("Collapse sidebar")}
              aria-expanded
              data-tip={tr("Collapse")}
              className={cn(RAIL_ICON_BUTTON_CLASS, "hidden sm:inline-flex")}
              type="button"
              onClick={() => setCollapsed(true)}
            >
              <Icon name="panel-left" className="size-[1.05rem]" />
            </button>
          )}
          <CloseButton
            className="sm:hidden"
            label={tr("Close sidebar")}
            onClick={() => setSidebarOpen(false)}
          />
        </div>

        {/* Search, new chat and the + menu: always in reach, above the
            scroller. Collapsed they stack as icons. */}
        {actions ? (
          <div
            className={[
              "shrink-0 pb-1.5 pl-[var(--rail-pad-l)] pr-2",
              collapsed ? "sm:flex sm:flex-col sm:items-center sm:gap-1 sm:px-0 sm:pt-2" : "",
            ].join(" ")}
          >
            {actions}
          </div>
        ) : null}

        {/* The one scroller. Everything between the search row and the
            account menu — the top items, Pinned, Projects, Chats — scrolls
            together, at every width; no list inside it is a scroll box of
            its own. The gutter is reserved only where a scrollbar takes
            room, so rows do not jump when a list grows past the window. */}
        <div
          ref={bodyRef}
          data-testid="rail-body"
          data-rail-body=""
          className={[
            "rail-scroll flex min-h-0 flex-1 flex-col overflow-y-auto overflow-x-hidden overscroll-contain pb-3 pl-[var(--rail-pad-l)] pr-[max(0px,calc(0.5rem-var(--rail-gutter)))] pt-0.5",
            collapsed ? "sm:items-center sm:px-0" : "",
          ].join(" ")}
        >
          {/* The top of the sidebar: Patrols, Background tasks and whatever
              the person keeps there, in their own order. */}
          <RailNav
            activeView={activeView}
            collapsed={collapsed}
            opsCount={opsCount}
            onNavigate={() => setSidebarOpen(false)}
          />
          {children ? (
            <>
              <div
                aria-hidden="true"
                className={["mx-2 my-2 h-px shrink-0 bg-[var(--color-border)]", collapsed ? "sm:mx-0 sm:w-6" : ""].join(" ")}
              />
              {children}
            </>
          ) : null}
        </div>

        {footer ? <div className="shrink-0 pl-[var(--rail-pad-l)] pr-2">{footer}</div> : null}

        <SidebarCustomizer />
        {/* The patrol wizard, for whatever on the page asks for it (the +
            menu, the Patrols page, a ?launchpad=1 link). */}
        <PatrolWizardHost onOpen={() => setSidebarOpen(false)} />

        <div
          className={[
            "shrink-0 pl-[var(--rail-pad-l)] pr-2",
            collapsed ? "sm:px-0" : "",
          ].join(" ")}
          style={{ paddingBottom: "max(0.5rem, env(safe-area-inset-bottom))" }}
        >
          <AccountMenu
            email={account.email}
            appName={brandName}
            onSignOut={account.onSignOut}
            railCollapsed={collapsed}
            current={activeView === "settings"}
            analyticsCurrent={activeView === "analytics"}
            guidesCurrent={activeView === "help"}
          />
        </div>

        {/* The sidebar's edge: drag it, or focus it and use the arrow keys.
            Enter puts the width back to the default. Not on a phone (the
            drawer has one width) and not in the icon strip. */}
        {resizable ? (
          // A focusable separator IS the window-splitter pattern (WAI-ARIA):
          // the role is interactive when it takes focus and reports a value.
          // eslint-disable-next-line jsx-a11y/no-noninteractive-element-interactions
          <div
            role="separator"
            aria-orientation="vertical"
            aria-label={tr("Resize sidebar")}
            aria-valuemin={RAIL_MIN_WIDTH}
            aria-valuemax={RAIL_MAX_WIDTH}
            aria-valuenow={width ?? RAIL_DEFAULT_WIDTH}
            // eslint-disable-next-line jsx-a11y/no-noninteractive-tabindex
            tabIndex={0}
            data-testid="rail-resize"
            data-dragging={dragging ? "true" : undefined}
            title={tr("Drag to resize the sidebar")}
            className="rail-resize absolute inset-y-0 -right-[5px] z-10 hidden w-[9px] cursor-col-resize touch-none sm:block any-pointer-coarse:hidden"
            onPointerDown={onResizePointerDown}
            onPointerMove={onResizePointerMove}
            onPointerUp={endResize}
            onPointerCancel={endResize}
            onDoubleClick={() => setWidth?.(null)}
            onKeyDown={onResizeKeyDown}
          />
        ) : null}
      </aside>
      {tip && tip.text && typeof document !== "undefined"
        ? createPortal(
            <span aria-hidden="true" className="rail-tip" style={{ top: tip.top, left: tip.left }}>
              {tip.text}
            </span>,
            document.body,
          )
        : null}
    </>
  );
}

export default NavRail;
