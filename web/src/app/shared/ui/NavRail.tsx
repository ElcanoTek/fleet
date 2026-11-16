import { t as tr } from "@/app/lib/i18n";
// NavRail — the shared left rail that unifies Chat and the Operations Center
// into one experience (#169). It owns the frame both surfaces render: brand,
// the Chat / Operations Center / Guides navigation (router links with active
// state), a surface-specific middle slot (`children`), an optional
// surface-specific `footer`, and the account menu. One rail for every surface —
// switching between them is ordinary navigation, so existing routing/auth are
// preserved.
//
// Responsive/collapse model (the design's .rail):
//   - <640px (below sm): the prior off-canvas drawer, toggled via sidebarOpen
//     with a tap-to-dismiss backdrop and the hamburger in PageTopBar. The
//     drawer always shows the full rail content regardless of collapse state.
//   - ≥sm: an in-flow sticky column. Expanded it is the design's 300px rail;
//     collapsed it is a 4.25rem icon strip (2.5rem icon-square nav items with
//     data-tip labels, icon-only new-chat buttons, avatar-only account menu).
//     The collapse toggle lives in the brand row; the preference persists to
//     localStorage["rail-collapsed"]. Width/padding animate on the fast motion
//     token, gated behind `railReady` so first paint never animates.
//   - 640–900px (`isNarrow`): the rail auto-collapses (state only — the stored
//     preference is untouched). Expanding it there opens the rail as a fixed
//     overlay over the content with a scrim (click or Escape dismisses, focus
//     returns to the toggle, Tab is trapped inside) instead of pushing the
//     layout.
//
// The owning shells call useRailCollapse() and pass the result in, so the
// surface content (conversation list, New task button) can adapt to the same
// state without a second source of truth.

import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { LaunchpadButton } from "@/app/launchpad/LaunchpadButton";
import { NavToChat, NavToOrchestrator } from "./CrossViewNav";
import { CloseButton } from "./CloseButton";
import { Icon } from "./Icon";
import { AccountMenu } from "./AccountMenu";
import { isInteractiveTarget } from "./interactiveTarget";
import { readPref, writePref } from "@/app/shared/lib/storage";

// "settings" renders the rail with neither surface active (the design's
// settings view): Chat/Operations Center stay plain links and the account
// button below takes the `current` tint instead. "analytics" and "help" are
// reached from the account menu, so they tint the account button too.
export type RailView = "chat" | "orchestrator" | "settings" | "analytics" | "help";

export const RAIL_COLLAPSED_STORAGE_KEY = "rail-collapsed";
const NARROW_QUERY = "(max-width: 900px)";

export type RailCollapse = {
  /** Icon-strip mode (≥sm). Auto-true at ≤900px; user-set persists. */
  collapsed: boolean;
  /** Viewport ≤900px — expanding the rail overlays the content with a scrim. */
  isNarrow: boolean;
  /** Gates the width/padding transition so first paint never animates. */
  railReady: boolean;
  setCollapsed: (next: boolean) => void;
};

// useRailCollapse owns the collapse state for a shell. All reads happen in the
// mount effect (never in the initializer) because the orchestrator SSRs its
// shell — the state starts expanded on both server and client and reconciles
// to the persisted preference before `railReady` enables the transition, so
// there is no hydration mismatch and no first-paint animation.
export function useRailCollapse(): RailCollapse {
  const [collapsed, setCollapsedState] = useState(false);
  const [isNarrow, setIsNarrow] = useState(false);
  const [railReady, setRailReady] = useState(false);

  useEffect(() => {
    const mql = window.matchMedia(NARROW_QUERY);
    const syncFromEnvironment = () => {
      // Private-mode / storage-disabled: default expanded.
      const stored = readPref(RAIL_COLLAPSED_STORAGE_KEY) === "1";
      setIsNarrow(mql.matches);
      // Narrow viewports auto-collapse without touching the stored preference.
      setCollapsedState(stored || mql.matches);
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

  return { collapsed, isNarrow, railReady, setCollapsed };
}

function navItemClass(active: boolean, collapsed: boolean): string {
  return [
    "group/nav relative flex items-center gap-2.5 rounded-[var(--radius-md)] px-2.5 py-2 text-[0.875rem] no-underline transition",
    active
      ? "bg-[color-mix(in_srgb,var(--color-primary)_18%,transparent)] font-semibold text-[var(--color-text-primary)] before:absolute before:left-0 before:top-1/2 before:h-[0.95rem] before:w-0.5 before:-translate-y-1/2 before:rounded-full before:bg-[var(--color-primary)]"
      : "text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]",
    // Collapsed strip (≥sm): 2.5rem squares with the hue tile centered.
    collapsed ? "sm:size-10 sm:justify-center sm:gap-0 sm:p-0" : "",
  ].join(" ");
}

// navTileClass — the icon sits in a small accent-tinted tile with a
// motion-safe hover lift; active saturates the tint with a soft glow. One
// brand hue for every surface (per-surface colors read as noise here — the
// status palette stays reserved for actual task states).
function navTileClass(active: boolean): string {
  const tint = active
    ? "bg-[color-mix(in_srgb,var(--color-accent)_24%,transparent)] text-[var(--color-accent)] shadow-[0_0_10px_color-mix(in_srgb,var(--color-accent)_24%,transparent)]"
    : "bg-[color-mix(in_srgb,var(--color-accent)_10%,transparent)] text-[color-mix(in_srgb,var(--color-accent)_75%,var(--color-text-secondary))]";
  return [
    "flex size-7 shrink-0 items-center justify-center rounded-[0.55rem] transition",
    "motion-safe:group-hover/nav:scale-110 group-hover/nav:text-[var(--color-text-primary)]",
    tint,
  ].join(" ");
}

// RailLabel — a rail item's name with its one-line subtitle. In the collapsed
// strip it is hidden from sight only (sm:sr-only): the hover data-tip is drawn
// by the stylesheet, which a screen reader does not announce, so with the
// label removed outright the icon was an unnamed link.
function RailLabel({ title, subtitle, collapsed }: { title: string; subtitle: string; collapsed: boolean }) {
  return (
    <span className={["flex min-w-0 flex-1 flex-col leading-tight", collapsed ? "sm:sr-only" : ""].join(" ")}>
      <span className="truncate">{title}</span>
      <span className="truncate text-[0.7rem] font-normal text-[var(--color-text-muted)]">{subtitle}</span>
    </span>
  );
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
  eyebrow = tr("Internal"),
  opsCount,
  sidebarOpen,
  setSidebarOpen,
  collapse,
  account,
  footer,
  children,
}: {
  activeView: RailView;
  brandName: string;
  brandLogoSrc?: string;
  eyebrow?: string;
  opsCount?: number;
  sidebarOpen: boolean;
  setSidebarOpen: (open: boolean) => void;
  collapse: RailCollapse;
  account: { email: string; onSignOut: () => void };
  footer?: ReactNode;
  children?: ReactNode;
}) {
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
        className={[
          // <sm: the off-canvas drawer (full rail content, translate toggled).
          // --sidebar-surface is a gradient, so it must land in
          // background-image (the un-hinted arbitrary-value form emits
          // background-color, which drops gradient values); --color-bg
          // underneath keeps the drawer opaque over scrolled content.
          "fixed inset-y-0 left-0 z-30 flex h-[100dvh] w-[min(19rem,85vw)] flex-col gap-2 border-r border-[var(--color-border)] bg-[var(--color-bg)] bg-[image:var(--sidebar-surface)] py-[0.85rem] shadow-[var(--shadow-lg)] backdrop-blur-xl transition-[translate,visibility] duration-base max-sm:overflow-hidden",
          // Closed on a phone it is also invisible, not only slid off-screen:
          // an off-screen drawer still took keyboard focus (the first Tab
          // stops on every page were its hidden items) and was still read
          // out. Visibility joins the transition, so it stays painted until
          // the slide-out has finished. (The property that moves it is
          // `translate`, which is what Tailwind's translate-x-* sets; naming
          // `transform` here left the slide with nothing to animate.)
          sidebarOpen ? "translate-x-0" : "-translate-x-full max-sm:invisible",
          // ≥sm: in-flow sticky column (or the fixed narrow overlay), design
          // widths: 300px expanded / 4.25rem collapsed strip. The design's
          // rail surface is the semi-transparent --sidebar-surface gradient
          // over the app-wide --gradient-bg on <body>.
          "sm:z-30 sm:translate-x-0 sm:bg-transparent sm:backdrop-blur-0",
          overlay
            ? "sm:fixed sm:inset-y-0 sm:left-0 sm:z-[390] sm:shadow-[var(--shadow-md)]"
            : "sm:sticky sm:top-0 sm:h-screen sm:shadow-none",
          // 300px at the default text size; it grows with the reader's own
          // font size (18.75rem), up to a third of the window.
          collapsed ? "sm:w-[4.25rem]" : "sm:w-[clamp(300px,18.75rem,34vw)]",
          railReady ? "sm:transition-[width,padding] sm:duration-fast" : "",
        ].join(" ")}
        style={{
          paddingLeft: `max(${collapsed ? "0.55rem" : "0.75rem"}, env(safe-area-inset-left))`,
          paddingRight: collapsed ? "0.55rem" : "0.75rem",
          paddingBottom: "max(0.85rem, env(safe-area-inset-bottom))",
        }}
      >
        {/* Brand row — logo + name, with the collapse toggle at the row's end;
            collapsed (≥sm) it stacks the logo above the toggle. */}
        <div
          className={[
            "flex items-center px-1",
            // justify-between stays on below sm in both states: narrow
            // viewports always report collapsed, and dropping it there parked
            // the drawer's close X against the brand name instead of at the
            // row's end.
            collapsed
              ? "justify-between sm:flex-col sm:justify-start sm:gap-2 sm:px-0"
              : "justify-between",
          ].join(" ")}
        >
          <div className={["flex min-w-0 items-center gap-2.5", collapsed ? "sm:flex-col" : ""].join(" ")}>
            {/* A plain <img>, never an optimizer: the bundle mark arrives as
                "/api/brand/logo" (no extension) and may be an SVG. Under
                next/image that combination rendered a broken image on every
                page; NavRail.test.tsx keeps the src pointing at the route. */}
            <img src={brandLogoSrc} alt={brandName} width={28} height={28} fetchPriority="high" decoding="async" style={{ color: "transparent" }} />
            <span className={["flex min-w-0 flex-col leading-[1.05]", collapsed ? "sm:hidden" : ""].join(" ")}>
              <span className="text-[0.6rem] uppercase tracking-[0.1em] text-[var(--color-text-muted)]">
                {eyebrow}
              </span>
              <span className="truncate font-heading text-[0.95rem] font-bold text-[var(--color-text-primary)]">
                {brandName}
              </span>
            </span>
          </div>
          <button
            ref={collapseToggleRef}
            aria-label={collapsed ? tr("Expand sidebar") : tr("Collapse sidebar")}
            aria-expanded={!collapsed}
            data-tip={collapsed ? tr("Expand") : tr("Collapse")}
            className={[
              "hidden shrink-0 items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] sm:inline-flex",
              collapsed ? "size-10" : "ml-auto size-[1.9rem]",
            ].join(" ")}
            type="button"
            onClick={() => setCollapsed(!collapsed)}
          >
            <Icon name="panel-left" className="size-4" />
          </button>
          <CloseButton
            className="sm:hidden"
            label={tr("Close sidebar")}
            onClick={() => setSidebarOpen(false)}
          />
        </div>

        {/* Primary navigation */}
        <nav
          aria-label={tr("Switch surface")}
          className={["grid grid-cols-[minmax(0,1fr)] gap-0.5", collapsed ? "sm:justify-items-center" : ""].join(" ")}
        >
          {/* The three ways of working, each with a subtitle that says what
              makes it different — patrols and background tasks both run on a
              schedule, so the names alone would blur: a patrol is a chat you
              can talk to; a background task runs headless, with no chat (and
              emails its result only when it has recipients). (The user guides live in the account menu.) */}
          <NavToChat
            className={navItemClass(activeView === "chat", collapsed)}
            ariaCurrent={activeView === "chat" ? "page" : undefined}
            dataTip={collapsed ? tr("Chat — talk it through") : undefined}
          >
            <span className={navTileClass(activeView === "chat")} aria-hidden="true">
              <Icon name="message" className="size-[1rem]" />
            </span>
            <RailLabel title={tr("Chat")} subtitle={tr("Talk it through")} collapsed={collapsed} />
          </NavToChat>
          {/* Patrols (docs/PATROL-LAUNCHPAD.md): patrols live in Chat; this item
              is their index (a menu of your patrol chats) and, with none yet,
              the Patrol Launchpad wizard that sets one up. */}
          <LaunchpadButton
            className={`${navItemClass(false, collapsed)} w-full cursor-pointer text-left`}
            tileClassName={navTileClass(false)}
            label={<RailLabel title={tr("Patrols")} subtitle={tr("Chats that check in on a schedule")} collapsed={collapsed} />}
            dataTip={collapsed ? tr("Patrols — chats that check in on a schedule") : undefined}
            collapsed={collapsed}
            onOpen={() => setSidebarOpen(false)}
          />
          <NavToOrchestrator
            className={navItemClass(activeView === "orchestrator", collapsed)}
            ariaCurrent={activeView === "orchestrator" ? "page" : undefined}
            dataTip={
              collapsed
                ? typeof opsCount === "number" && opsCount > 0
                  ? tr("Background tasks · {0} running", [opsCount])
                  : tr("Background tasks · Runs without a chat")
                : undefined
            }
          >
            <span className={navTileClass(activeView === "orchestrator")} aria-hidden="true">
              <Icon name="grid" className="size-[1rem]" />
            </span>
            <RailLabel title={tr("Background tasks")} subtitle={tr("Runs without a chat")} collapsed={collapsed} />
            {typeof opsCount === "number" && opsCount > 0 ? (
              <span
                className={[
                  "font-[family-name:var(--font-code)] text-[0.7rem] text-[var(--color-text-muted)]",
                  collapsed ? "sm:hidden" : "",
                ].join(" ")}
              >
                {opsCount}
              </span>
            ) : null}
          </NavToOrchestrator>
        </nav>

        <div className="my-1 h-px shrink-0 bg-[var(--color-border)]" />

        {/* Surface-specific middle content (chat: conversation org; ops: New task). */}
        <div className="flex min-h-0 flex-1 flex-col">{children}</div>

        {/* Surface-specific footer items (e.g. update banner, delete-all-unpinned). */}
        {footer}

        <AccountMenu
          email={account.email}
          appName={brandName}
          onSignOut={account.onSignOut}
          railCollapsed={collapsed}
          current={activeView === "settings"}
          analyticsCurrent={activeView === "analytics"}
          guidesCurrent={activeView === "help"}
        />
      </aside>
    </>
  );
}

export default NavRail;
