import type { ReactNode } from "react";

// StatusChip — the one pill/badge recipe for small status-tinted labels
// ("Connected", "Bundled", "Third-party", health pills, …). Pages used to
// hand-roll this trio (`rounded-full border px-2 py-0.5` + border/tint/text
// colors) with raw hex values; the colors now ride the semantic
// --color-*-strong / --color-*-soft token pairs in globals.css so every chip
// themes with light/dark automatically. See web/src/app/DESIGN.md.
//
// Purely presentational: a <span>, no behavior. Pass `className` for layout
// concerns only (margins, truncation) — not to re-color a tone.

export type StatusTone = "success" | "warning" | "danger" | "neutral";

const TONE_CLASS: Record<StatusTone, string> = {
  success:
    "border-[var(--color-success-strong)] bg-[color-mix(in_srgb,var(--color-success-strong)_15%,transparent)] text-[var(--color-success-soft)]",
  warning:
    "border-[var(--color-warning-strong)] bg-[color-mix(in_srgb,var(--color-warning-strong)_15%,transparent)] text-[var(--color-warning-soft)]",
  danger:
    "border-[var(--color-danger-strong)] bg-[color-mix(in_srgb,var(--color-danger-strong)_15%,transparent)] text-[var(--color-danger-soft)]",
  neutral:
    "border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] text-[var(--color-text-secondary)]",
};

// statusToneClass exposes just the color trio for the rare surface that needs
// the tone on different base markup (e.g. a non-pill badge).
export function statusToneClass(tone: StatusTone): string {
  return TONE_CLASS[tone];
}

export function StatusChip({
  tone,
  children,
  className,
  title,
}: {
  tone: StatusTone;
  children: ReactNode;
  className?: string;
  title?: string;
}) {
  return (
    <span
      title={title}
      className={`inline-block whitespace-nowrap rounded-full border px-2 py-0.5 text-[0.6875rem] ${TONE_CLASS[tone]}${className ? ` ${className}` : ""}`}
    >
      {children}
    </span>
  );
}
