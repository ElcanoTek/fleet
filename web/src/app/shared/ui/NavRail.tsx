"use client";

// NavRail — the shared left rail that unifies Chat and the Operations Center
// into one experience (#169). It owns the frame both surfaces render: brand,
// the Chat / Operations Center navigation (router links with active state), a
// surface-specific middle slot (`children`), an optional surface-specific
// `footer`, and the account menu. Two routes, one rail — switching surfaces is
// ordinary navigation, so existing routing/auth are preserved.
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

import Image from "next/image";
import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { NavToChat, NavToOrchestrator } from "./CrossViewNav";
import { Icon } from "./Icon";
import { AccountMenu } from "./AccountMenu";

export type RailView = "chat" | "orchestrator";

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
      let stored = false;
      try {
        stored = window.localStorage.getItem(RAIL_COLLAPSED_STORAGE_KEY) === "1";
      } catch {
        // Private-mode / storage-disabled: default expanded.
      }
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
    try {
      window.localStorage.setItem(RAIL_COLLAPSED_STORAGE_KEY, next ? "1" : "0");
    } catch {
      // Private-mode / storage-disabled: state stays session-only.
    }
  }, []);

  return { collapsed, isNarrow, railReady, setCollapsed };
}

function navItemClass(active: boolean, collapsed: boolean): string {
  return [
    "group/nav relative flex items-center gap-2.5 rounded-[var(--radius-md)] px-2.5 py-2 text-[0.875rem] no-underline transition",
    active
      ? "bg-[color-mix(in_srgb,var(--color-primary)_18%,transparent)] font-semibold text-[var(--color-text-primary)] before:absolute before:left-0 before:top-1/2 before:h-[0.95rem] before:w-0.5 before:-translate-y-1/2 before:rounded-full before:bg-[var(--color-primary)]"
      : "text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]",
    // Collapsed strip (≥sm): 2.5rem icon squares, centered.
    collapsed ? "sm:size-10 sm:justify-center sm:gap-0 sm:p-0" : "",
  ].join(" ");
}

function navIconClass(active: boolean): string {
  return ["size-[1.05rem] shrink-0", active ? "text-[var(--color-primary)]" : "text-[var(--color-accent)]"].join(" ");
}

// Focusable elements inside the overlay drawer, for the Tab trap. Mirrors the
// selector Menu.tsx uses.
function overlayFocusables(container: HTMLElement): HTMLElement[] {
  return Array.from(
    container.querySelectorAll<HTMLElement>(
      'button:not([disabled]), input:not([disabled]), [href], [tabindex]:not([tabindex="-1"])',
    ),
  ).filter((el) => el.offsetParent !== null);
}

export function NavRail({
  activeView,
  brandName,
  brandLogoSrc = "/logos/elcano-mark-primary.svg",
  eyebrow = "Internal",
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
  account: { email: string; onSignOut: () => void; onSettings?: () => void };
  footer?: ReactNode;
  children?: ReactNode;
}) {
  const { collapsed, isNarrow, railReady, setCollapsed } = collapse;
  // Expanded on a narrow (640–900px) viewport → fixed overlay + scrim.
  const overlay = isNarrow && !collapsed;
  const collapseToggleRef = useRef<HTMLButtonElement | null>(null);
  const asideRef = useRef<HTMLElement | null>(null);

  // Escape dismisses the narrow overlay and returns focus to the toggle
  // (open menus stop the event before it reaches this window listener).
  useEffect(() => {
    if (!overlay) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape") return;
      setCollapsed(true);
      collapseToggleRef.current?.focus();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [overlay, setCollapsed]);

  // Tab trap while the overlay is up — it behaves as a modal drawer. Portaled
  // menus manage their own trap, so this only cycles the rail's own controls.
  const onAsideKeyDown = (e: React.KeyboardEvent<HTMLElement>) => {
    if (!overlay || e.key !== "Tab") return;
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

  return (
    <>
      {/* Mobile backdrop — taps outside the drawer dismiss it. */}
      <button
        aria-label="Close navigation"
        className={[
          "fixed inset-0 z-20 bg-[color-mix(in_srgb,var(--color-overlay-strong)_120%,black)] backdrop-blur-[2px] transition sm:hidden",
          sidebarOpen ? "block" : "hidden",
        ].join(" ")}
        type="button"
        onClick={() => setSidebarOpen(false)}
      />

      {/* Narrow-viewport scrim (the design's .rail-scrim): shown behind the
          expanded overlay rail at 640–900px; click to dismiss. */}
      {overlay ? (
        <button
          aria-label="Close navigation"
          type="button"
          className="fixed inset-0 z-[380] hidden bg-[color-mix(in_srgb,var(--color-overlay-strong)_120%,black)] sm:block"
          onClick={() => setCollapsed(true)}
        />
      ) : null}

      <aside
        ref={asideRef}
        aria-label="Primary navigation"
        onKeyDown={onAsideKeyDown}
        className={[
          // <sm: the off-canvas drawer (full rail content, translate toggled).
          "fixed inset-y-0 left-0 z-30 flex h-[100dvh] w-[min(19rem,85vw)] flex-col gap-2 border-r border-[var(--color-border)] bg-[color-mix(in_srgb,var(--sidebar-surface)_96%,black)] py-[0.85rem] shadow-[var(--shadow-lg)] backdrop-blur-xl transition-transform duration-base max-sm:overflow-hidden",
          sidebarOpen ? "translate-x-0" : "-translate-x-full",
          // ≥sm: in-flow sticky column (or the fixed narrow overlay), design
          // widths: 300px expanded / 4.25rem collapsed strip.
          "sm:z-30 sm:translate-x-0 sm:bg-[var(--sidebar-surface)] sm:backdrop-blur-0",
          overlay
            ? "sm:fixed sm:inset-y-0 sm:left-0 sm:z-[390] sm:shadow-[var(--shadow-md)]"
            : "sm:sticky sm:top-0 sm:h-screen sm:shadow-none",
          collapsed ? "sm:w-[4.25rem]" : "sm:w-[300px]",
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
            collapsed ? "sm:flex-col sm:gap-2 sm:px-0" : "justify-between",
          ].join(" ")}
        >
          <div className={["flex min-w-0 items-center gap-2.5", collapsed ? "sm:flex-col" : ""].join(" ")}>
            <Image src={brandLogoSrc} alt={brandName} width={28} height={28} priority />
            <span className={["flex min-w-0 flex-col leading-[1.05]", collapsed ? "sm:hidden" : ""].join(" ")}>
              <span className="text-[0.6rem] uppercase tracking-[0.1em] text-[var(--color-text-muted)]">
                {eyebrow}
              </span>
              <span className="truncate font-heading text-[0.9375rem] font-semibold text-[var(--color-text-primary)]">
                {brandName}
              </span>
            </span>
          </div>
          <button
            ref={collapseToggleRef}
            aria-label={collapsed ? "Expand sidebar" : "Collapse sidebar"}
            aria-expanded={!collapsed}
            data-tip={collapsed ? "Expand" : "Collapse"}
            className={[
              "hidden shrink-0 items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] sm:inline-flex",
              collapsed ? "size-10" : "ml-auto size-[1.9rem]",
            ].join(" ")}
            type="button"
            onClick={() => setCollapsed(!collapsed)}
          >
            <Icon name="panel-left" className="size-4" />
          </button>
          <button
            aria-label="Close sidebar"
            className="inline-flex size-11 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] sm:hidden"
            type="button"
            onClick={() => setSidebarOpen(false)}
          >
            <Icon name="close" className="size-4" />
          </button>
        </div>

        {/* Primary navigation */}
        <nav
          aria-label="Switch surface"
          className={["grid gap-0.5", collapsed ? "sm:justify-items-center" : ""].join(" ")}
        >
          <NavToChat
            className={navItemClass(activeView === "chat", collapsed)}
            ariaCurrent={activeView === "chat" ? "page" : undefined}
            dataTip={collapsed ? "Chat" : undefined}
          >
            <Icon name="message" className={navIconClass(activeView === "chat")} />
            <span className={["min-w-0 flex-1 truncate", collapsed ? "sm:hidden" : ""].join(" ")}>Chat</span>
          </NavToChat>
          <NavToOrchestrator
            className={navItemClass(activeView === "orchestrator", collapsed)}
            ariaCurrent={activeView === "orchestrator" ? "page" : undefined}
            dataTip={
              collapsed
                ? typeof opsCount === "number" && opsCount > 0
                  ? `Operations Center · ${opsCount} running`
                  : "Operations Center"
                : undefined
            }
          >
            <Icon name="grid" className={navIconClass(activeView === "orchestrator")} />
            <span className={["min-w-0 flex-1 truncate", collapsed ? "sm:hidden" : ""].join(" ")}>
              Operations Center
            </span>
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
          onSignOut={account.onSignOut}
          onSettings={account.onSettings}
          railCollapsed={collapsed}
        />
      </aside>
    </>
  );
}

export default NavRail;
