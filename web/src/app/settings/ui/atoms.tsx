import { t as tr } from "@/app/lib/i18n";
// Settings-area atoms (fleet-unified settings design pass). These transcribe
// the design's component vocabulary — .btn/.settings-input/.conn-badge/
// .set-switch/.theme-seg/.act-status and friends — into the token-driven
// Tailwind idiom the rest of the app uses, with the design's exact metrics.
// Every color rides a semantic token so client theming keeps working.

import { useEffect, useRef, useState, type ReactNode } from "react";
import { Icon } from "@/app/shared/ui/Icon";
import { StatusChip, type StatusTone } from "@/app/shared/ui/StatusChip";
import { Button, buttonVariants } from "@/components/ui/button";
import { Switch } from "@/components/ui/switch";
import { cn } from "@/lib/utils";

/* ── Buttons (.btn / .btn-primary / .btn-ghost / .btn-sm / .conn-reveal) ── */

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
  const size = sm ? "sm" : "default";
  if (variant === "primary") {
    return buttonVariants({ variant: "default", size });
  }
  if (armed) {
    return buttonVariants({ variant: "destructive", size });
  }
  if (danger) {
    return cn(
      buttonVariants({ variant: "ghost", size }),
      "border-transparent hover:border-(--color-status-error-border) hover:bg-(--color-status-error-bg) hover:text-(--color-status-error-fg)",
    );
  }
  return cn(
    buttonVariants({ variant: "ghost", size }),
    reveal ? "border-border" : "border-transparent",
  );
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
    <Button
      type="button"
      variant="ghost"
      size="sm"
      aria-expanded={open}
      disabled={disabled}
      data-testid={testId}
      onClick={onClick}
    >
      <Icon name={open ? "close" : "plus"} className="size-[0.85rem]" />
      {open ? (openLabel ?? tr("Cancel")) : closedLabel}
    </Button>
  );
}

/* ── Inputs (.settings-input) ── */

export const SETTINGS_INPUT =
  "min-h-[2.5rem] w-full rounded-md border border-input bg-(--color-overlay-soft) px-3 py-2 text-body-md text-foreground [transition:var(--transition-fast)] outline-none placeholder:text-muted-foreground focus-visible:border-(--color-accent) focus-visible:shadow-(--focus-ring) disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive";

/* ── Badges (.conn-badge + variants) ── */

// ConnBadge keeps the settings pages' variant names but renders the shared
// StatusChip, so a settings badge and an Operations Center status are the
// same size and family.
export type BadgeVariant = "neutral" | "success" | "warn" | "overridden";

const BADGE_TONE: Record<BadgeVariant, StatusTone> = {
  neutral: "neutral",
  success: "success",
  warn: "warning",
  overridden: "accent",
};

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
  return (
    <StatusChip tone={BADGE_TONE[variant]} title={title} className={["shrink-0", className ?? ""].join(" ").trim()}>
      {children}
    </StatusChip>
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
        <span className="text-[0.9rem] font-semibold text-[var(--color-text-primary)]">
          {label}
        </span>
        {desc ? (
          // A div, not a span: a description may hold block content (a
          // details expander, a list).
          <div className="max-w-[32rem] text-[0.78rem] leading-[1.5] text-[var(--color-text-muted)]">
            {desc}
          </div>
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
  // The bundled-connector cards render the switch at compact scale
  // (.conn-toggle .set-switch).
  small?: boolean;
  testId?: string;
}) {
  return (
    <Switch
      checked={on}
      onCheckedChange={() => onToggle()}
      aria-label={label}
      disabled={disabled}
      data-testid={testId}
      size={small ? "default" : "lg"}
    />
  );
}

/* ── Segmented control: lives in shared/ui so every surface uses one ── */

export { Segmented } from "@/app/shared/ui/Segmented";

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
    <span
      role="status"
      className={["text-[0.74rem]", color, className ?? ""].join(" ")}
    >
      {children}
    </span>
  );
}

export function ActNote({ children }: { children: ReactNode }) {
  return (
    <span className="text-[0.72rem] text-[var(--color-text-muted)]">
      {children}
    </span>
  );
}

/* ── Two-click inline-confirmed danger button ── */

// InlineConfirmButton — first click arms ("Confirm delete" in danger colors),
// second click fires; blur or Escape disarms. The design brief's
// inline-confirm affordance, composed from the ghost-danger button.
export function InlineConfirmButton({
  label,
  confirmLabel = tr("Confirm delete"),
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
    <Button
      type="button"
      variant={armed ? "destructive" : "ghost"}
      size="sm"
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
      className={
        armed
          ? undefined
          : "border-transparent hover:border-(--color-status-error-border) hover:bg-(--color-status-error-bg) hover:text-(--color-status-error-fg)"
      }
    >
      {armed ? confirmLabel : label}
    </Button>
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
          {expanded ? tr("less") : tr("more")}
        </button>
      ) : null}
    </div>
  );
}
