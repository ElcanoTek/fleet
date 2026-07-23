"use client";

import { Icon } from "./Icon";

// CloseButton is THE dismiss "×" for dialogs, drawers, and panels — one box
// (2rem, .hit-area extends the touch target), one glyph size, one rounding,
// one hover (a slight red tint: dismissal reads as "abort", softer than the
// full red the destructive trash buttons use). Chip removes and input-clear
// buttons are a different, deliberately smaller pattern and don't use this.
// `className` is for layout-only extras (order, responsive visibility).
export function CloseButton({
  label,
  onClick,
  className,
  testId,
}: {
  label: string;
  onClick: () => void;
  className?: string;
  testId?: string;
}) {
  return (
    <button
      type="button"
      aria-label={label}
      data-testid={testId}
      className={[
        "hit-area inline-flex size-8 shrink-0 items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-status-error-bg)] hover:text-[var(--color-status-error-fg)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]",
        className ?? "",
      ]
        .join(" ")
        .trim()}
      onClick={onClick}
    >
      <Icon name="close" className="size-4" />
    </button>
  );
}
