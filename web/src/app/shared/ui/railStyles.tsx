import type { ReactNode } from "react";

// The sidebar's shared look (docs/SIDEBAR.md): one row for everything in it —
// Patrols, Background tasks, a kept dashboard, a pinned chat, a project, a
// chat — so the whole column reads as one list. A row is an icon (where the
// kind has one), the title, and at most one quiet status at the right edge.
//
// Row height comes from --rail-row-h, which the rail sets from the person's
// density (Comfortable or Compact), so every row follows one setting. A touch
// screen always gets a 44px row.

/** A row's box: height, radius, hover and active fills. */
export function railRowClass(active: boolean, extra = ""): string {
  return [
    "relative flex min-h-[var(--rail-row-h)] w-full min-w-0 items-center gap-2.5 rounded-[var(--radius-md)] text-left text-[length:var(--rail-row-text)] leading-tight no-underline transition-colors any-pointer-coarse:min-h-11",
    active
      ? "bg-[var(--rail-active)] font-medium text-[var(--color-text-primary)]"
      : "text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]",
    "focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]",
    extra,
  ].join(" ");
}

/**
 * navItemClass — a row among the sidebar's top items. Collapsed (the icon
 * strip, from sm up) it is a 2.5rem square with the icon centered.
 */
export function navItemClass(active: boolean, collapsed: boolean): string {
  return railRowClass(active, ["px-2.5", collapsed ? "sm:size-10 sm:min-h-0 sm:justify-center sm:gap-0 sm:px-0" : ""].join(" "));
}

/** The leading icon of a row: one quiet weight, a step below the title. */
export function railIconClass(active = false): string {
  return ["size-[1.05rem] shrink-0", active ? "text-[var(--color-text-primary)]" : "text-[var(--color-text-secondary)] opacity-80"].join(" ");
}

/** A section's heading: "Pinned", "Projects", "Chats". */
export const RAIL_HEADING_CLASS =
  "text-[0.72rem] font-semibold tracking-[0.06em] text-[var(--color-text-muted)]";

/** The quiet text action at a heading's right edge ("Reorder"). */
export const RAIL_HEADING_ACTION_CLASS =
  "inline-flex h-6 shrink-0 items-center gap-1 rounded-md px-1.5 text-[0.75rem] font-medium text-[var(--color-link)] transition hover:bg-[var(--rail-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:h-9";

/** A small square icon button in the sidebar's chrome (gear, collapse, a heading's menu). */
export const RAIL_ICON_BUTTON_CLASS =
  "hit-area-touch inline-flex size-8 shrink-0 items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-muted)] transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]";

// RailLabel — a row's name. In the collapsed strip it is hidden from sight
// only (sm:sr-only): the hover data-tip is drawn by the stylesheet, which a
// screen reader does not announce, so with the label removed outright the
// icon was an unnamed link.
export function RailLabel({ title, collapsed, trailing }: { title: string; collapsed: boolean; trailing?: ReactNode }) {
  return (
    <span className={["flex min-w-0 flex-1 items-center gap-1.5", collapsed ? "sm:sr-only" : ""].join(" ")}>
      <span className="min-w-0 truncate">{title}</span>
      {trailing}
    </span>
  );
}
