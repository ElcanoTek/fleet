import type { ComponentProps, HTMLAttributes, ReactNode } from "react";
import { t } from "@/app/lib/i18n";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

const RUN_STATE_LABELS: Readonly<Record<string, string>> = {
  pending: t("pending"), scheduled: t("scheduled"), assigned: t("assigned"),
  leased: t("leased"), running: t("running"), proposed: t("proposed"),
  success: t("success"), succeeded: t("succeeded"), active: t("active"), idle: t("idle"),
  error: t("error"), failed: t("failed"), blocked: t("blocked"), cancelled: t("cancelled"),
  stopped: t("stopped"), paused: t("paused"), queued: t("queued"), completed: t("completed"),
  paused_awaiting_wake: t("paused_awaiting_wake"), paused_awaiting_input: t("paused_awaiting_input"),
  dead_lettered: t("dead_lettered"), warning: t("warning"), skipped: t("skipped"), unknown: t("unknown"),
};

/** Display known lifecycle states without changing IDs or custom labels. */
export function runStateLabel(state: string): string {
  return Object.hasOwn(RUN_STATE_LABELS, state) ? RUN_STATE_LABELS[state] : state;
}

// StatusChip — the one pill/badge recipe for small status-tinted labels
// ("Connected", "Bundled", "Third-party", health pills, task run states, SLA
// and log badges, …), backed by Flag's @flag/badge primitive while preserving
// Fleet's general card-safe -strong/-soft tints and run-blocked lifecycle state.
export type GeneralTone = "success" | "warning" | "danger" | "neutral" | "accent";
export type RunTone =
  | "run-pending"
  | "run-scheduled"
  | "run-assigned"
  | "run-leased"
  | "run-running"
  | "run-proposed"
  | "run-success"
  | "run-warning"
  | "run-error"
  | "run-blocked"
  | "run-cancelled";
export type StatusTone = GeneralTone | RunTone;

type BadgeVariant = NonNullable<ComponentProps<typeof Badge>["variant"]>;

const BADGE_VARIANT_BY_TONE: Partial<Record<StatusTone, BadgeVariant>> = {
  "run-pending": "pending",
  "run-scheduled": "scheduled",
  "run-assigned": "assigned",
  "run-leased": "leased",
  "run-running": "running",
  "run-proposed": "analyzing",
  "run-success": "success",
  "run-warning": "warning",
  "run-error": "error",
  "run-cancelled": "cancelled",
};

// Spelled out, not built from a template string, so Tailwind's source scan
// sees every arbitrary class literally.
const TONE_CLASS: Record<StatusTone, string> = {
  success:
    "border-[var(--color-success-strong)] bg-[color-mix(in_srgb,var(--color-success-strong)_15%,transparent)] text-[var(--color-success-soft)]",
  warning:
    "border-[var(--color-warning-strong)] bg-[color-mix(in_srgb,var(--color-warning-strong)_15%,transparent)] text-[var(--color-warning-soft)]",
  danger:
    "border-[var(--color-danger-strong)] bg-[color-mix(in_srgb,var(--color-danger-strong)_15%,transparent)] text-[var(--color-danger-soft)]",
  neutral:
    "border-[var(--color-border-strong)] bg-[var(--color-overlay-soft)] text-[var(--color-text-secondary)]",
  accent:
    "border-[color-mix(in_srgb,var(--color-accent)_50%,transparent)] bg-[color-mix(in_srgb,var(--color-accent)_12%,transparent)] text-[var(--color-accent)]",
  "run-pending":
    "border-[var(--color-status-pending-border)] bg-[var(--color-status-pending-bg)] text-[var(--color-status-pending-fg)]",
  "run-scheduled":
    "border-[var(--color-status-scheduled-border)] bg-[var(--color-status-scheduled-bg)] text-[var(--color-status-scheduled-fg)]",
  "run-assigned":
    "border-[var(--color-status-assigned-border)] bg-[var(--color-status-assigned-bg)] text-[var(--color-status-assigned-fg)]",
  "run-leased":
    "border-[var(--color-status-leased-border)] bg-[var(--color-status-leased-bg)] text-[var(--color-status-leased-fg)]",
  "run-running":
    "border-[var(--color-status-running-border)] bg-[var(--color-status-running-bg)] text-[var(--color-status-running-fg)]",
  "run-proposed":
    "border-[var(--color-status-proposed-border)] bg-[var(--color-status-proposed-bg)] text-[var(--color-status-proposed-fg)]",
  "run-success":
    "border-[var(--color-status-success-border)] bg-[var(--color-status-success-bg)] text-[var(--color-status-success-fg)]",
  "run-warning":
    "border-[var(--color-status-warning-border)] bg-[var(--color-status-warning-bg)] text-[var(--color-status-warning-fg)]",
  "run-error":
    "border-[var(--color-status-error-border)] bg-[var(--color-status-error-bg)] text-[var(--color-status-error-fg)]",
  "run-blocked":
    "border-[var(--color-status-blocked-border)] bg-[var(--color-status-blocked-bg)] text-[var(--color-status-blocked-fg)]",
  "run-cancelled":
    "border-[var(--color-status-cancelled-border)] bg-[var(--color-status-cancelled-bg)] text-[var(--color-status-cancelled-fg)]",
};

// statusToneClass exposes just the color trio for the rare surface that needs
// the tone on different base markup (e.g. a non-pill badge).
export function statusToneClass(tone: StatusTone): string {
  return TONE_CLASS[tone];
}

// runStateTone maps a task/run lifecycle value to its color family — the
// mapping the old `.status-<state>` CSS rules encoded.
export function runStateTone(state: string): RunTone {
  switch (state) {
    case "success":
    case "succeeded":
    case "active":
    case "idle":
      return "run-success";
    case "error":
    case "failed":
      return "run-error";
    case "running":
      return "run-running";
    case "proposed":
      return "run-proposed";
    case "scheduled":
      return "run-scheduled";
    case "assigned":
      return "run-assigned";
    // A sleeping self-wake task is calm, not a warning: it re-queues itself
    // (docs/SELF-WAKE.md), unlike paused_awaiting_input which needs a human.
    case "leased":
    case "paused_awaiting_wake":
      return "run-leased";
    case "dead_lettered":
    case "paused_awaiting_input":
      return "run-warning";
    // A success whose completion clause recorded a blocked outcome: it did its
    // job and decided not to publish, which must not read as the same green.
    case "blocked":
      return "run-blocked";
    case "cancelled":
    case "stopped":
      return "run-cancelled";
    default:
      return "run-pending";
  }
}

export function StatusChip({
  tone,
  caps = false,
  children,
  className,
  title,
  testId,
  ...rest
}: {
  tone: StatusTone;
  /** Uppercase + tracked, for run/lifecycle states (system values scanned
   *  down a table column). Descriptive badges keep normal case. */
  caps?: boolean;
  children: ReactNode;
  className?: string;
  title?: string;
  testId?: string;
} & Omit<HTMLAttributes<HTMLSpanElement>, "className" | "children" | "title">) {
  const mappedVariant = BADGE_VARIANT_BY_TONE[tone];
  return (
    <Badge
      {...rest}
      variant={mappedVariant ?? "outline"}
      title={title}
      data-testid={testId}
      className={cn(
        "px-2 py-0.5 text-[0.6875rem]",
        caps && "font-semibold uppercase tracking-[0.06em]",
        !mappedVariant && TONE_CLASS[tone],
        className,
      )}
    >
      {children}
    </Badge>
  );
}

// RunStatusChip is a lifecycle state on the shared chip. The `status-badge
// status-<state>` classes stay as the stable hooks tests and e2e selectors use.
export function RunStatusChip({
  state,
  children,
  title,
  testId,
  className,
}: {
  state: string;
  children?: ReactNode;
  title?: string;
  testId?: string;
  className?: string;
}) {
  return (
    <StatusChip
      tone={runStateTone(state)}
      caps
      title={title}
      testId={testId}
      className={`status-badge status-${state}${className ? ` ${className}` : ""}`}
    >
      {children == null || children === state ? runStateLabel(state) : children}
    </StatusChip>
  );
}
