"use client";

// NavRail — the shared left rail that unifies Chat and the Operations Center
// into one experience (#169). It owns the frame both surfaces render: brand,
// the Chat / Operations Center navigation (router links with active state), a
// surface-specific middle slot (`children`), an optional surface-specific
// `footer`, and the account menu. Two routes, one rail — switching surfaces is
// ordinary navigation, so existing routing/auth are preserved.
//
// Responsive behavior matches the prior chat sidebar: a left drawer under `lg`
// (toggled via sidebarOpen) and a sticky in-flow column at `lg`+.

import Image from "next/image";
import type { ReactNode } from "react";
import { NavToChat, NavToOrchestrator } from "./CrossViewNav";
import { Icon } from "./Icon";
import { AccountMenu } from "./AccountMenu";

export type RailView = "chat" | "orchestrator";

function navItemClass(active: boolean): string {
  return [
    "group/nav relative flex items-center gap-2.5 rounded-[var(--radius-md)] px-2.5 py-2 text-[0.875rem] no-underline transition",
    active
      ? "bg-[color-mix(in_srgb,var(--color-primary)_18%,transparent)] font-semibold text-[var(--color-text-primary)] before:absolute before:left-0 before:top-1/2 before:h-[0.95rem] before:w-0.5 before:-translate-y-1/2 before:rounded-full before:bg-[var(--color-primary)]"
      : "text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]",
  ].join(" ");
}

function navIconClass(active: boolean): string {
  return ["size-[1.05rem] shrink-0", active ? "text-[var(--color-primary)]" : "text-[var(--color-accent)]"].join(" ");
}

export function NavRail({
  activeView,
  brandName,
  brandLogoSrc = "/logos/elcano-mark-primary.svg",
  eyebrow = "Internal",
  opsCount,
  sidebarOpen,
  setSidebarOpen,
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
  account: { email: string; onSignOut: () => void; onSettings?: () => void };
  footer?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <>
      {/* Mobile backdrop — taps outside the drawer dismiss it. */}
      <button
        aria-label="Close navigation"
        className={[
          "fixed inset-0 z-20 bg-[color-mix(in_srgb,var(--color-overlay-strong)_120%,black)] backdrop-blur-[2px] transition lg:hidden",
          sidebarOpen ? "block" : "hidden",
        ].join(" ")}
        type="button"
        onClick={() => setSidebarOpen(false)}
      />

      <aside
        aria-label="Primary navigation"
        className={[
          "fixed inset-y-0 left-0 z-30 flex h-[100dvh] w-[min(19rem,85vw)] flex-col gap-2 overflow-hidden border-r border-[var(--color-border)] bg-[color-mix(in_srgb,var(--sidebar-surface)_96%,black)] px-3 py-4 shadow-[var(--shadow-lg)] backdrop-blur-xl transition-transform duration-200 sm:w-[min(18rem,calc(100vw-2.5rem))] sm:bg-[var(--sidebar-surface)] lg:sticky lg:h-screen lg:w-auto lg:translate-x-0 lg:border-r-0 lg:bg-[var(--sidebar-surface)] lg:shadow-none lg:backdrop-blur-0",
          sidebarOpen ? "translate-x-0" : "-translate-x-full",
        ].join(" ")}
        style={{
          paddingLeft: "max(0.75rem, env(safe-area-inset-left))",
          paddingBottom: "max(1rem, env(safe-area-inset-bottom))",
        }}
      >
        {/* Brand */}
        <div className="flex items-center justify-between px-1">
          <div className="flex items-center gap-2.5">
            <Image src={brandLogoSrc} alt={brandName} width={28} height={28} priority />
            <span className="flex flex-col leading-[1.05]">
              <span className="text-[0.6rem] uppercase tracking-[0.1em] text-[var(--color-text-muted)]">
                {eyebrow}
              </span>
              <span className="font-heading text-[0.9375rem] font-semibold text-[var(--color-text-primary)]">
                {brandName}
              </span>
            </span>
          </div>
          <button
            aria-label="Close sidebar"
            className="inline-flex size-11 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] sm:size-7 lg:hidden"
            type="button"
            onClick={() => setSidebarOpen(false)}
          >
            <Icon name="close" className="size-4" />
          </button>
        </div>

        {/* Primary navigation */}
        <nav aria-label="Switch surface" className="grid gap-0.5">
          <NavToChat
            className={navItemClass(activeView === "chat")}
            ariaCurrent={activeView === "chat" ? "page" : undefined}
          >
            <Icon name="message" className={navIconClass(activeView === "chat")} />
            <span className="min-w-0 flex-1">Chat</span>
          </NavToChat>
          <NavToOrchestrator
            className={navItemClass(activeView === "orchestrator")}
            ariaCurrent={activeView === "orchestrator" ? "page" : undefined}
          >
            <Icon name="grid" className={navIconClass(activeView === "orchestrator")} />
            <span className="min-w-0 flex-1">Operations Center</span>
            {typeof opsCount === "number" && opsCount > 0 ? (
              <span className="font-[family-name:var(--font-code)] text-[0.7rem] text-[var(--color-text-muted)]">
                {opsCount}
              </span>
            ) : null}
          </NavToOrchestrator>
        </nav>

        <div className="my-1 h-px bg-[var(--color-border)]" />

        {/* Surface-specific middle content (chat: conversation org; ops: New task). */}
        <div className="flex min-h-0 flex-1 flex-col">{children}</div>

        {/* Surface-specific footer items (e.g. update banner, delete-all-unpinned). */}
        {footer}

        <AccountMenu email={account.email} onSignOut={account.onSignOut} onSettings={account.onSettings} />
      </aside>
    </>
  );
}

export default NavRail;
