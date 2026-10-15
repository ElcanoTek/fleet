"use client";

import { useId } from "react";

// Segmented is the one single-choice switch for 2–4 mutually exclusive
// options (theme, schedule mode, list/week, memory views, the email preview's
// inbox and size). The look lives in globals.css (.seg / .seg-option) so the
// orchestrator's CSS-classed radiogroups and tablists render identically;
// this component is the button-group (aria-pressed) form of it.


export function Segmented<T extends string>({
  value,
  options,
  onChange,
  label,
  disabled,
  dividers,
  emphasized,
  onDeselect,
}: {
  value: T;
  options: readonly { value: T; label: string; description?: string }[];
  onChange: (next: T) => void;
  label: string;
  disabled?: boolean;
  /** Hairline separators between segments — for controls with 3+ textual
   *  options where the boundaries otherwise blur together. */
  dividers?: boolean;
  /** Draw the control's border with the branded action gradient. */
  emphasized?: boolean;
  /** Optional checkbox-like behavior for a selected one-option control. */
  onDeselect?: () => void;
}) {
  const tooltipBaseId = useId();
  return (
    <span
      role="group"
      aria-label={label}
      className={[
        "seg",
        emphasized
          ? "border-transparent bg-[linear-gradient(var(--color-surface-1),var(--color-surface-1))_padding-box,var(--gradient-action-primary)_border-box]"
          : "",
        dividers ? "divide-x divide-[var(--color-border-subtle)]" : "",
      ].join(" ")}
    >
      {options.map((o, index) => {
        const tooltipId = o.description
          ? `${tooltipBaseId}-option-${index}`
          : undefined;
        return (
          <span key={o.value} className="group/seg-option relative inline-flex">
            <button
              type="button"
              aria-pressed={value === o.value}
              aria-describedby={tooltipId}
              disabled={disabled}
              onClick={() => value === o.value && onDeselect ? onDeselect() : onChange(o.value)}
              className="seg-option"
            >
              {o.label}
            </button>
            {o.description ? (
              <span
                id={tooltipId}
                role="tooltip"
                className="pointer-events-none absolute left-1/2 top-[calc(100%+0.4rem)] z-[500] w-max max-w-[min(16rem,calc(100vw-1rem))] -translate-x-1/2 rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] px-[0.55rem] py-[0.4rem] text-center text-[0.7rem] font-normal leading-snug text-[var(--color-text-secondary)] opacity-0 shadow-[var(--shadow-md)] transition-opacity duration-75 group-hover/seg-option:opacity-100 group-focus-within/seg-option:opacity-100"
              >
                {o.description}
              </span>
            ) : null}
          </span>
        );
      })}
    </span>
  );
}
