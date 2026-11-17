import type { ReactNode } from "react";

// Rail item styling shared by NavRail and the configurable nav list
// (sidebar/RailNav.tsx): one look for Chat, Patrols, Background tasks and
// every shortcut a person pins.

export function navItemClass(active: boolean, collapsed: boolean): string {
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
export function navTileClass(active: boolean): string {
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
export function RailLabel({ title, subtitle, collapsed, trailing }: { title: string; subtitle?: string; collapsed: boolean; trailing?: ReactNode }) {
  return (
    <span className={["flex min-w-0 flex-1 flex-col leading-tight", collapsed ? "sm:sr-only" : ""].join(" ")}>
      <span className="flex min-w-0 items-center gap-1.5">
        <span className="truncate">{title}</span>
        {trailing}
      </span>
      {subtitle ? (
        <span className="truncate text-[0.7rem] font-normal text-[var(--color-text-muted)]">{subtitle}</span>
      ) : null}
    </span>
  );
}

