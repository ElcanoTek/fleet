"use client";

import type { ReactNode } from "react";

// PageTopBar — the shared top header for Chat and the Operations Center (#169).
// One bordered bar so the two views read identically: a mobile sidebar toggle +
// the view title on the left, and optional right-aligned actions. The
// Operations Center renders it with just a title; Chat renders it with the same
// title treatment plus its header controls (search, shortcuts, memories,
// details) in the actions slot. This is the design handoff's
// .topbar / .topbar-inner / .topbar-title pattern, expressed with the same
// semantic tokens the rest of the app uses.

export type PageTopBarProps = {
  title: string;
  // Opens the off-canvas conversation/nav sidebar on narrow viewports.
  onMenu: () => void;
  // Right-aligned controls (Chat supplies its header buttons here; Ops omits).
  actions?: ReactNode;
};

export function PageTopBar({ title, onMenu, actions }: PageTopBarProps) {
  return (
    <header className="flex items-center justify-between gap-3 border-b border-[var(--color-border)] px-4 pb-3 pt-[max(0.75rem,env(safe-area-inset-top))] sm:px-6">
      {/* min-h matches the action-button size (size-11 / sm:size-8) so the bar
          is the same height whether or not a view supplies actions — Chat (with
          its header controls) and the Operations Center (none) line up exactly,
          with no height/title jump when switching between them. */}
      <div className="flex min-h-11 min-w-0 items-center gap-3 sm:min-h-8">
        <button
          aria-label="Open sidebar"
          className="inline-flex size-9 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] lg:hidden"
          type="button"
          onClick={onMenu}
        >
          <svg
            aria-hidden="true"
            className="size-4"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.9"
            strokeLinecap="round"
            strokeLinejoin="round"
          >
            <path d="M4 6h16" />
            <path d="M4 12h16" />
            <path d="M4 18h16" />
          </svg>
        </button>
        <h1 className="truncate text-[1.05rem] font-semibold text-[var(--color-text-primary)]">
          {title}
        </h1>
      </div>
      {actions ? <div className="flex items-center gap-1">{actions}</div> : null}
    </header>
  );
}

export default PageTopBar;
