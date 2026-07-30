"use client";

// Settings-area atoms (fleet-unified settings design pass). These transcribe
// the design's component vocabulary — .btn/.settings-input/.conn-badge/
// .set-switch/.theme-seg/.act-status and friends — into the token-driven
// Tailwind idiom the rest of the app uses, with the design's exact metrics.
// Every color rides a semantic token so client theming keeps working.

import { useEffect, useRef, useState, type ReactNode } from "react";
import { Icon } from "@/app/shared/ui/Icon";

/* ── Buttons (.btn / .btn-primary / .btn-ghost / .btn-sm / .conn-reveal) ── */

// NOTE on composition: Tailwind v4 emits same-property utilities in a fixed
// (value-sorted) order, so "base class + appended override" does NOT cascade
// like inline CSS — the emitted order decides. btnClass therefore computes
// exactly ONE class per contested property (border-color, text-color,
// hover background) instead of stacking overrides.
const BTN_BASE =
  "inline-flex items-center justify-center gap-[0.4rem] rounded-[var(--radius-md)] border font-medium transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:cursor-not-allowed disabled:opacity-50";

export function btnClass({
  variant = "ghost",
  sm = false,
  reveal = false,
  danger = false,
  armed = false,
}: {
  variant?: "primary" | "ghost";
  sm?: boolean;
  reveal?: boolean;
  danger?: boolean;
  // The InlineConfirmButton's armed state: danger colors applied NOW, not on
  // hover.
  armed?: boolean;
} = {}): string {
  const size = sm
    ? "px-[0.7rem] py-[0.3rem] text-[0.78rem]"
    : "px-[0.85rem] py-[0.45rem] text-[0.85rem]";
  const border = armed
    ? "border-[color-mix(in_srgb,var(--color-danger)_45%,transparent)]"
    : reveal
      ? "border-[var(--color-border)]"
      : "border-transparent";
  let color: string;
  if (variant === "primary") {
    color = "bg-[var(--color-primary)] text-[var(--color-on-primary)] hover:bg-[var(--color-primary-hover)]";
  } else if (armed) {
    color =
      "bg-[color-mix(in_srgb,var(--color-danger)_12%,transparent)] text-[var(--color-danger)]";
  } else if (danger) {
    color =
      "text-[var(--color-text-secondary)] hover:bg-[color-mix(in_srgb,var(--color-danger)_12%,transparent)] hover:text-[var(--color-danger)]";
  } else {
    color =
      "text-[var(--color-text-secondary)] hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]";
  }
  return [BTN_BASE, size, border, color].join(" ");
}

// RevealButton — the design's "＋ Add …" / "× Cancel" panel-header toggle
// (.btn.btn-ghost.btn-sm.conn-reveal with the icon swapping on open).
export function RevealButton({
  open,
  openLabel,
  closedLabel,
  onClick,
  disabled,
  testId,
}: {
  open: boolean;
  openLabel?: string;
  closedLabel: string;
  onClick: () => void;
  disabled?: boolean;
  testId?: string;
}) {
  return (
    <button
      type="button"
      aria-expanded={open}
      disabled={disabled}
      data-testid={testId}
      onClick={onClick}
      className={btnClass({ sm: true, reveal: true })}
    >
      <Icon name={open ? "close" : "plus"} className="size-[0.85rem]" />
      {open ? (openLabel ?? "Cancel") : closedLabel}
    </button>
  );
}

/* ── Inputs (.settings-input) ── */

export const SETTINGS_INPUT =
  "min-h-[2.6rem] w-full rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-[0.7rem] py-2 text-[0.95rem] text-[var(--color-text-primary)] outline-none placeholder:text-[var(--color-text-muted)] focus-visible:border-[var(--color-border-strong)] focus-visible:shadow-[var(--focus-ring)]";

/* ── Badges (.conn-badge + variants) ── */

export type BadgeVariant = "neutral" | "success" | "warn" | "overridden";

export function ConnBadge({
  variant = "neutral",
  className,
  title,
  children,
}: {
  variant?: BadgeVariant;
  className?: string;
  title?: string;
  children: ReactNode;
}) {
  const variantClass = {
    neutral: "border-[var(--color-border-strong)] text-[var(--color-text-muted)]",
    success: "border-[var(--color-success-border)] text-[var(--color-success)]",
    warn: "border-[color-mix(in_srgb,var(--color-warning)_50%,transparent)] text-[var(--color-warning)]",
    overridden:
      "border-[color-mix(in_srgb,var(--color-accent)_50%,transparent)] bg-[color-mix(in_srgb,var(--color-accent)_10%,transparent)] text-[var(--color-accent)]",
  }[variant];
  return (
    <span
      title={title}
      className={[
        "shrink-0 whitespace-nowrap rounded-[var(--radius-pill)] border px-2 py-[0.13rem] text-[0.63rem] font-semibold tracking-[0.04em]",
        variantClass,
        className ?? "",
      ].join(" ")}
    >
      {children}
    </span>
  );
}

/* ── Preference rows (.set-row) ── */

export function SetRow({
  label,
  desc,
  children,
}: {
  label: string;
  desc?: ReactNode;
  children: ReactNode;
}) {
  return (
    <div className="flex items-center gap-8 border-b border-[var(--color-border-subtle)] py-4 last:border-b-0 max-[640px]:flex-col max-[640px]:items-start max-[640px]:gap-[0.7rem]">
      <div className="grid min-w-0 flex-1 gap-[0.3rem]">
        <span className="text-[0.9rem] font-semibold text-[var(--color-text-primary)]">{label}</span>
        {desc ? (
          <span className="max-w-[32rem] text-[0.78rem] leading-[1.5] text-[var(--color-text-muted)]">
            {desc}
          </span>
        ) : null}
      </div>
      <div className="flex shrink-0 items-center">{children}</div>
    </div>
  );
}

/* ── Switch (.set-switch) ── */

export function SetSwitch({
  on,
  onToggle,
  label,
  disabled,
  small = false,
  testId,
}: {
  on: boolean;
  onToggle: () => void;
  label: string;
  disabled?: boolean;
  // The bundled-connector cards render the switch at 0.88 scale
  // (.conn-toggle .set-switch).
  small?: boolean;
  testId?: string;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={on}
      aria-label={label}
      disabled={disabled}
      data-testid={testId}
      onClick={onToggle}
      className={[
        "relative h-[1.3rem] w-[2.3rem] shrink-0 cursor-pointer rounded-full border-none p-0 transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:cursor-not-allowed disabled:opacity-50",
        on
          ? "bg-[var(--color-primary)]"
          : "bg-[color-mix(in_srgb,var(--color-primary)_32%,transparent)]",
        small ? "origin-left scale-[0.88]" : "",
      ].join(" ")}
    >
      <span
        className={[
          "absolute left-[3px] top-[3px] size-[calc(1.3rem-6px)] rounded-full transition",
          on ? "translate-x-4 bg-white" : "bg-[var(--color-text-muted)]",
        ].join(" ")}
      />
    </button>
  );
}

/* ── Segmented control (.theme-seg) ── */

export function Segmented<T extends string>({
  value,
  options,
  onChange,
  label,
  disabled,
}: {
  value: T;
  options: readonly { value: T; label: string }[];
  onChange: (next: T) => void;
  label: string;
  disabled?: boolean;
}) {
  return (
    <span
      role="group"
      aria-label={label}
      className="inline-flex shrink-0 overflow-hidden rounded-[var(--radius-pill)] border border-[var(--color-border)]"
    >
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          aria-pressed={value === o.value}
          disabled={disabled}
          onClick={() => onChange(o.value)}
          className={[
            "px-[0.6rem] py-[0.18rem] text-[0.72rem] font-medium transition focus-visible:outline-none disabled:cursor-not-allowed disabled:opacity-50",
            value === o.value
              ? "bg-[var(--color-primary)] text-[var(--color-on-primary)]"
              : "text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)] focus-visible:bg-[var(--color-overlay-soft)]",
          ].join(" ")}
        >
          {o.label}
        </button>
      ))}
    </span>
  );
}

/* ── Inline action status (.act-status / .act-note) ── */

export function ActStatus({
  state,
  children,
  className,
}: {
  state: "running" | "ok" | "err";
  children: ReactNode;
  className?: string;
}) {
  const color = {
    running: "text-[var(--color-accent)]",
    ok: "text-[var(--color-success)]",
    err: "text-[var(--color-danger)]",
  }[state];
  return (
    <span role="status" className={["text-[0.74rem]", color, className ?? ""].join(" ")}>
      {children}
    </span>
  );
}

export function ActNote({ children }: { children: ReactNode }) {
  return <span className="text-[0.72rem] text-[var(--color-text-muted)]">{children}</span>;
}

/* ── Two-click inline-confirmed danger button ── */

// InlineConfirmButton — first click arms ("Confirm delete" in danger colors),
// second click fires; blur or Escape disarms. The design brief's
// inline-confirm affordance, composed from the ghost-danger button.
export function InlineConfirmButton({
  label,
  confirmLabel = "Confirm delete",
  onConfirm,
  disabled,
  testId,
}: {
  label: string;
  confirmLabel?: string;
  onConfirm: () => void;
  disabled?: boolean;
  testId?: string;
}) {
  const [armed, setArmed] = useState(false);
  return (
    <button
      type="button"
      disabled={disabled}
      data-testid={testId}
      onClick={() => {
        if (armed) {
          setArmed(false);
          onConfirm();
        } else {
          setArmed(true);
        }
      }}
      onBlur={() => setArmed(false)}
      onKeyDown={(e) => {
        if (e.key === "Escape") setArmed(false);
      }}
      className={btnClass({ sm: true, danger: !armed, armed })}
    >
      {armed ? confirmLabel : label}
    </button>
  );
}

/* ── Inline code chip (.feat-prov code / .set-head p code …) ── */

export function CodeChip({ children }: { children: ReactNode }) {
  return (
    <code className="rounded-[0.3rem] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-[0.28rem] py-[0.02rem] font-[family-name:var(--font-code)] text-[0.92em] text-[var(--color-text-secondary)]">
      {children}
    </code>
  );
}

/* ── Clamped description with expand (.conn-desc + the brief's expand) ── */

// ClampText — the design clamps descriptions to 3 lines; the brief adds an
// expand affordance. The "more" toggle renders only when the text actually
// overflows its clamp (measured after layout, re-measured on resize).
export function ClampText({
  text,
  className,
}: {
  text: string;
  className?: string;
}) {
  const [expanded, setExpanded] = useState(false);
  const [clamped, setClamped] = useState(false);
  const ref = useRef<HTMLParagraphElement | null>(null);

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const measure = () => setClamped(el.scrollHeight > el.clientHeight + 1);
    measure();
    // jsdom (unit tests) has no ResizeObserver — the initial measure is all
    // it gets there; browsers re-measure on width changes.
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, [text, expanded]);

  return (
    <div className="min-w-0">
      <p
        ref={ref}
        className={[
          "m-0 text-[0.75rem] leading-[1.5] text-[var(--color-text-secondary)]",
          expanded ? "" : "line-clamp-3",
          className ?? "",
        ].join(" ")}
      >
        {text}
      </p>
      {clamped || expanded ? (
        <button
          type="button"
          aria-expanded={expanded}
          onClick={() => setExpanded((e) => !e)}
          className="mt-[0.15rem] border-none bg-transparent p-0 text-[0.72rem] text-[var(--color-accent)] underline-offset-2 hover:underline focus-visible:shadow-[var(--focus-ring)] focus-visible:outline-none"
        >
          {expanded ? "less" : "more"}
        </button>
      ) : null}
    </div>
  );
}
