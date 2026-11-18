import { t as tr } from "@/app/lib/i18n";
import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { Icon } from "./Icon";
import { PatrolSkippedNote } from "./PatrolSkippedNote";
import {
  type ChatSchedule,
  type ConversationApprovalMode,
  resolveApprovalMode,
} from "./history";
import { StatusChip } from "@/app/shared/ui/StatusChip";
import {
  describeCronExpression,
  describeCronExpressionShort,
} from "@/app/shared/lib/cron";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { DropdownMenu as DropdownMenuPrimitive } from "radix-ui";

export function formatScheduleCadence(sc: ChatSchedule): string {
  if (sc.kind === "timer") {
    const secs = sc.duration_seconds ?? 0;
    if (secs >= 3600 && secs % 3600 === 0) return tr("One-shot ({0}h)", [secs / 3600]);
    if (secs >= 60) return tr("One-shot ({0}m)", [Math.round(secs / 60)]);
    if (secs > 0) return tr("One-shot ({0}s)", [secs]);
    return tr("One-shot timer");
  }
  const cron = (sc.cron ?? sc.cron_expr ?? "").trim();
  if (!cron) return tr("Recurring patrol");
  if (cron === "0 * * * *" || cron === "0 */1 * * *") return tr("Every hour");
  const stepHours = cron.match(/^0 \*\/(\d+) \* \* \*$/);
  if (stepHours) return tr("Every {0}h", [stepHours[1]]);
  const stepMins = cron.match(/^\*\/(\d+) \* \* \* \*$/);
  if (stepMins) return tr("Every {0}m", [stepMins[1]]);
  const daily = cron.match(/^(\d+) (\d+) \* \* \*$/);
  if (daily) {
    const hh = daily[2].padStart(2, "0");
    const mm = daily[1].padStart(2, "0");
    return tr("Daily at {0}:{1}", [hh, mm]);
  }
  const weekdays = cron.match(/^(\d+) (\d+) \* \* (?:1-5|MON-FRI)$/i);
  if (weekdays) {
    const hh = weekdays[2].padStart(2, "0");
    const mm = weekdays[1].padStart(2, "0");
    return tr("Weekdays at {0}:{1}", [hh, mm]);
  }
  return (
    describeCronExpressionShort(cron) ||
    describeCronExpression(cron) ||
    cron
  );
}

export function formatCountdown(nextRunAt: number, nowSec: number): string {
  if (!nextRunAt || nextRunAt <= 0) return "";
  const diff = nextRunAt - nowSec;
  if (diff <= 0) return tr("due now");
  if (diff < 60) return tr("in {0}s", [diff]);
  const mins = Math.floor(diff / 60);
  if (mins < 60) return tr("in {0}m", [mins]);
  const hours = Math.floor(mins / 60);
  const remMins = mins % 60;
  if (hours < 24) return remMins > 0 ? tr("in {0}h {1}m", [hours, remMins]) : tr("in {0}h", [hours]);
  const days = Math.floor(hours / 24);
  return tr("in {0}d {1}h", [days, hours % 24]);
}

/** The patrols the bar and the Patrols dialog show: armed or paused. */
export function liveSchedules(schedules: ChatSchedule[]): ChatSchedule[] {
  return schedules.filter((s) => s.status === "active" || s.status === "paused");
}

/** One status line for a patrol: "Weekdays at 08:00 · in 19h 53m · Run #7",
 *  or "Paused · Weekdays at 08:00 · Run #7". */
export function formatScheduleMeta(sc: ChatSchedule, nowSec: number): string {
  const cadence = formatScheduleCadence(sc);
  const runs =
    (sc.max_runs ?? 0) > 0 && sc.kind !== "timer"
      ? tr("Run {0}/{1}", [sc.run_count, sc.max_runs])
      : sc.run_count > 0
        ? tr("Run #{0}", [sc.run_count])
        : "";
  const parts =
    sc.status === "paused"
      ? [tr("Paused"), cadence]
      : [cadence, formatCountdown(sc.next_run_at, nowSec)];
  if (runs) parts.push(runs);
  return parts.filter(Boolean).join(" · ");
}

/** The schedule a pending approval most likely came from: the one that ran
 *  last. Approvals are per conversation, not per schedule, so this is where
 *  the "waiting for your approval" note is shown. */
export function attentionScheduleId(schedules: ChatSchedule[]): string | null {
  let best: ChatSchedule | null = null;
  for (const s of schedules) {
    if (!best || (s.last_run_at ?? 0) > (best.last_run_at ?? 0)) best = s;
  }
  return best?.id ?? null;
}

export function pendingApprovalText(count: number): string {
  return count === 1
    ? tr("An action is waiting for your approval")
    : tr("{0} actions are waiting for your approval", [count]);
}

// The approval modes in the order the controls list them. The descriptions
// say what each mode really does (the composer's Approval mode menu and the
// guide use the same words), not the design mock's shorthand.
export const APPROVAL_MODE_OPTIONS: ReadonlyArray<{
  id: ConversationApprovalMode;
  label: string;
  desc: string;
}> = [
  {
    id: "manual",
    label: tr("Manual"),
    desc: tr("Waits on interactive approval cards for every critical action."),
  },
  {
    id: "auto",
    label: tr("Auto-approve"),
    desc: tr("Skips cards for routine workspace actions; checked plans and external emails still ask."),
  },
  {
    id: "autonomous",
    label: tr("Autonomous"),
    desc: tr("Runs live platform actions unattended with receipt cards; external messages, schedules, destructive changes, and git push still ask."),
  },
];

export function approvalModeLabel(mode: ConversationApprovalMode): string {
  return APPROVAL_MODE_OPTIONS.find((o) => o.id === mode)?.label ?? mode;
}

function boltClass(mode: ConversationApprovalMode): string {
  return mode === "autonomous"
    ? "text-[var(--color-warning-icon)]"
    : mode === "auto"
      ? "text-[var(--color-accent)]"
      : "text-[var(--color-text-muted)]";
}

// The bar's approval-mode dropdown. Switching an armed patrol to Autonomous
// asks once more inside the menu, since its next run will act without
// stopping; the deployment's one-time Autonomous risk notice (when not yet
// accepted) still follows, from onChange.
function ApprovalModeMenu({
  mode,
  anyArmed,
  onChange,
}: {
  mode: ConversationApprovalMode;
  anyArmed: boolean;
  onChange: (mode: ConversationApprovalMode) => void;
}) {
  const [open, setOpen] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const label = approvalModeLabel(mode);
  return (
    <DropdownMenu
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (!next) setConfirming(false);
      }}
    >
      <DropdownMenuTrigger asChild>
        <button
          type="button"
          data-testid="patrol-mode-menu"
          aria-label={tr("Approval mode: {0}", [label])}
          className={`inline-flex h-8 shrink-0 items-center gap-1.5 rounded-[var(--radius-md)] border bg-[var(--color-surface-2)] pr-2 pl-2.5 text-[0.8125rem] font-medium text-[var(--color-text-primary)] transition hover:border-[var(--color-border-strong)] ${
            open
              ? "border-[var(--color-selected-border)]"
              : "border-[var(--color-border)]"
          }`}
        >
          <Icon name="zap" className={`size-3 ${boltClass(mode)}`} />
          <span>{label}</span>
          <Icon name="chevron-down" className="size-3.5 text-[var(--color-text-muted)]" />
        </button>
      </DropdownMenuTrigger>
      <DropdownMenuContent
        side="top"
        align="end"
        sideOffset={8}
        data-testid="patrol-mode-menu-content"
        className="w-[min(21.5rem,calc(100vw-2rem))] p-1.5"
      >
        <DropdownMenuLabel className="px-2.5 pt-2 pb-1.5 font-[family-name:var(--font-code)] text-[0.6875rem] font-medium tracking-[0.1em] text-[var(--color-text-subtle)] uppercase">
          {tr("Approval mode")}
        </DropdownMenuLabel>
        <DropdownMenuRadioGroup value={mode}>
          {APPROVAL_MODE_OPTIONS.map((opt) => {
            const selected = opt.id === mode;
            const pending = confirming && opt.id === "autonomous";
            return (
              <DropdownMenuPrimitive.RadioItem
                key={opt.id}
                value={opt.id}
                data-testid={`patrol-mode-${opt.id}`}
                className={`relative flex cursor-pointer items-start gap-2.5 rounded-[var(--radius-md)] border p-2.5 outline-hidden select-none focus:shadow-[inset_0_0_0_1px_var(--color-row-active-border)] ${
                  pending
                    ? "border-[var(--color-warning-border)] bg-[var(--color-warning-tint)]"
                    : selected
                      ? "border-transparent bg-[var(--color-selected-bg)]"
                      : "border-transparent"
                }`}
                onSelect={(e) => {
                  if (opt.id === "autonomous" && mode !== "autonomous" && anyArmed) {
                    e.preventDefault();
                    setConfirming(true);
                    return;
                  }
                  if (opt.id !== mode) onChange(opt.id);
                }}
              >
                <span className="grid min-w-0 flex-1 gap-0.5 text-left">
                  <span className="text-[0.8125rem] font-semibold text-[var(--color-text-primary)]">
                    {opt.label}
                  </span>
                  <span className="text-[0.75rem] leading-[1.4] text-[var(--color-text-muted)]">
                    {opt.desc}
                  </span>
                </span>
                <Icon
                  name="check"
                  className={`mt-0.5 size-4 shrink-0 text-[var(--color-accent)] ${
                    selected ? "visible" : "invisible"
                  }`}
                />
              </DropdownMenuPrimitive.RadioItem>
            );
          })}
        </DropdownMenuRadioGroup>
        {confirming ? (
          <div
            role="alert"
            data-testid="patrol-mode-confirm"
            className="m-1 mt-1.5 grid gap-2.5 rounded-[var(--radius-md)] border border-[var(--color-warning-border)] bg-[var(--color-warning-tint)] p-2.5 pl-3"
          >
            <span className="text-[0.78rem] leading-[1.45] text-[var(--color-warning-text)]">
              {tr("Switch this running patrol to Autonomous? Its next run takes live platform actions without stopping for approval cards.")}
            </span>
            <div className="flex flex-wrap justify-end gap-2">
              <DropdownMenuItem
                className="h-8 shrink-0 justify-center rounded-[var(--radius-md)] border border-[var(--color-border-strong)] px-2.5 text-[0.8125rem] font-medium whitespace-nowrap"
                onSelect={(e) => {
                  e.preventDefault();
                  setConfirming(false);
                }}
              >
                {tr("Keep current")}
              </DropdownMenuItem>
              <DropdownMenuItem
                data-testid="patrol-mode-confirm-autonomous"
                className="h-8 shrink-0 justify-center whitespace-nowrap rounded-[var(--radius-md)] border border-[var(--color-warning-border)] bg-[color-mix(in_srgb,var(--color-warning)_28%,transparent)] px-2.5 text-[0.8125rem] font-semibold text-[var(--color-warning-text)] focus:bg-[color-mix(in_srgb,var(--color-warning)_36%,transparent)] focus:text-[var(--color-warning-text)]"
                onSelect={() => onChange("autonomous")}
              >
                {tr("Switch to Autonomous")}
              </DropdownMenuItem>
            </div>
          </div>
        ) : null}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

// The Patrol bar above the composer: one row per armed or paused patrol with
// its status, the approval-mode dropdown, Run now and Pause/Resume, and a
// Hide button. Hide only folds the bar into the composer's clock (the patrol
// keeps running); stopping a patrol lives in the Patrols & timers dialog,
// behind a second click, so no single click on the bar can end one (a
// Raptive trader cancelled a patrol with the old "×", 2026-11-17).
export function PatrolBanner({
  schedules,
  isAutoApprove,
  approvalMode,
  isStreaming,
  pendingApprovals = 0,
  onChangeApprovalMode,
  onRunNow,
  onTogglePause,
  onHide,
  onReview,
}: {
  schedules: ChatSchedule[];
  isAutoApprove?: boolean;
  approvalMode?: ConversationApprovalMode;
  isStreaming?: boolean;
  /** Pending approval cards in this conversation; > 0 shows the note. */
  pendingApprovals?: number;
  onChangeApprovalMode?: (mode: ConversationApprovalMode) => void;
  onRunNow: (scheduleId: string) => void | Promise<void>;
  onTogglePause: (scheduleId: string, currentStatus: ChatSchedule["status"]) => void | Promise<void>;
  onHide: () => void;
  onReview?: () => void;
}) {
  const effectiveApprovalMode = resolveApprovalMode(approvalMode, isAutoApprove);
  const visible = liveSchedules(schedules);
  const [nowSec, setNowSec] = useState<number>(() => Math.floor(Date.now() / 1000));
  // Every schedule with a request in flight (see PatrolsDialog's runBusy).
  const [busyIds, setBusyIds] = useState<ReadonlySet<string>>(() => new Set());
  // The ref refuses a second action on a schedule already in flight, even
  // one clicked before the disabled state rendered.
  const busyRef = useRef<Set<string>>(new Set());
  const runBusy = async (id: string, fn: () => unknown) => {
    if (busyRef.current.has(id)) return;
    busyRef.current.add(id);
    setBusyIds((prev) => new Set(prev).add(id));
    try {
      await fn();
    } finally {
      busyRef.current.delete(id);
      setBusyIds((prev) => {
        const next = new Set(prev);
        next.delete(id);
        return next;
      });
    }
  };

  useEffect(() => {
    if (visible.length === 0) return;
    const id = window.setInterval(() => {
      setNowSec(Math.floor(Date.now() / 1000));
    }, 10_000);
    return () => window.clearInterval(id);
  }, [visible.length]);

  if (visible.length === 0) return null;
  const anyArmed = visible.some((s) => s.status === "active");
  const attentionId = pendingApprovals > 0 ? attentionScheduleId(visible) : null;

  return (
    <div
      className="mx-auto mb-1.5 flex w-full max-w-[53rem] flex-col gap-1.5 px-1 sm:px-0"
      data-testid="patrol-banner"
    >
      {visible.map((sc, idx) => {
        const isPaused = sc.status === "paused";
        const attention = sc.id === attentionId;
        const busy = busyIds.has(sc.id) || Boolean(isStreaming);
        // A paused schedule can still run once now (the server allows it).
        const runDisabled = busy;
        return (
          <div
            key={sc.id}
            role="region"
            aria-label={sc.kind === "timer" ? tr("Active timer") : tr("Active patrol")}
            className={`rounded-[var(--radius-lg)] border bg-[color-mix(in_srgb,var(--color-accent)_6%,var(--color-surface-2))] text-xs text-[var(--color-text-secondary)] shadow-xs ${
              attention
                ? "border-[var(--color-warning-border)]"
                : "border-[color-mix(in_srgb,var(--color-accent)_34%,var(--color-border))]"
            }`}
          >
            <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 py-2 pr-2 pl-3.5">
              <div className="flex min-w-0 flex-[1_1_14rem] items-center gap-3">
                <span
                  aria-hidden="true"
                  data-testid="patrol-status-dot"
                  className={`inline-block size-2 shrink-0 rounded-full ${
                    isPaused
                      ? "border-2 border-[var(--color-text-muted)] bg-transparent"
                      : attention
                        ? "bg-[var(--color-warning)] shadow-[0_0_0_3px_var(--color-warning-tint)]"
                        : "animate-pulse bg-[var(--color-status-success-fg,#10b981)]"
                  }`}
                />
                <StatusChip tone={isPaused ? "neutral" : "accent"} caps className="shrink-0">
                  {sc.kind === "timer" ? tr("timer") : tr("patrol")}
                </StatusChip>
                <div className="grid min-w-0 flex-1 gap-0.5">
                  <span
                    className="min-w-0 truncate text-[0.875rem] font-semibold text-[var(--color-text-primary)]"
                    title={sc.prompt}
                  >
                    {sc.title}
                  </span>
                  <span className="min-w-0 truncate text-[0.75rem] text-[var(--color-text-muted)]">
                    {formatScheduleMeta(sc, nowSec)}
                  </span>
                </div>
              </div>

              <div className="ml-auto flex shrink-0 items-center gap-1.5">
                {/* The mode is the conversation's, so with several patrols
                    only the first row carries the control. */}
                {onChangeApprovalMode && idx === 0 ? (
                  <ApprovalModeMenu
                    mode={effectiveApprovalMode}
                    anyArmed={anyArmed}
                    onChange={onChangeApprovalMode}
                  />
                ) : null}
                <button
                  type="button"
                  disabled={runDisabled}
                  onClick={() => runBusy(sc.id, () => onRunNow(sc.id))}
                  className="pill-btn inline-flex h-8 items-center gap-1.5 font-medium disabled:opacity-40"
                >
                  <Icon name="play" className="size-3" />
                  {tr("Run now")}
                </button>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => runBusy(sc.id, () => onTogglePause(sc.id, sc.status))}
                  className="pill-btn inline-flex h-8 items-center gap-1.5 disabled:opacity-40"
                >
                  <Icon name={isPaused ? "play" : "pause"} className="size-3" />
                  {isPaused ? tr("Resume") : tr("Pause")}
                </button>
                {/* A waiting approval keeps the bar on screen, so Hide could
                    not fold it: offered again once the approval is settled. */}
                {pendingApprovals > 0 ? null : (
                  <>
                    <span aria-hidden="true" className="mx-0.5 h-5 w-px shrink-0 bg-[var(--color-border-strong)]" />
                    <button
                      type="button"
                      data-testid="patrol-bar-hide"
                      aria-label={tr("Hide patrol bar. The patrol keeps running.")}
                      data-tip-top={tr("Hide bar · patrol keeps running")}
                      data-tip-align="end"
                      onClick={onHide}
                      className="hit-area inline-flex size-8 shrink-0 items-center justify-center rounded-[var(--radius-md)] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
                    >
                      <Icon name="collapse-down" className="size-4" />
                    </button>
                  </>
                )}
              </div>
            </div>
            <PatrolSkippedNote schedule={sc} />
            {attention ? (
              <div
                data-testid="patrol-bar-attention"
                className="mx-2 mb-2 flex items-center gap-2.5 rounded-[var(--radius-md)] border border-[var(--color-warning-border)] bg-[var(--color-warning-tint)] py-1.5 pr-1.5 pl-3"
              >
                <Icon name="alert-circle" className="size-4 shrink-0 text-[var(--color-warning-icon)]" />
                <span className="min-w-0 flex-1 text-[0.8125rem] text-[var(--color-warning-text)]">
                  {pendingApprovalText(pendingApprovals)}
                </span>
                {onReview ? (
                  <button
                    type="button"
                    onClick={onReview}
                    className="h-[30px] shrink-0 rounded-[var(--radius-md)] border border-[var(--color-warning-border)] px-3 text-[0.8125rem] font-medium text-[var(--color-warning-text)] transition hover:bg-[var(--color-warning-tint)]"
                  >
                    {tr("Review")}
                  </button>
                ) : null}
              </div>
            ) : null}
          </div>
        );
      })}
    </div>
  );
}

// The note that pops above the composer's clock right after the bar is
// hidden: the patrol is still running, here is where it went, and Undo.
// It centers on the clock, then shifts to stay a 1rem gutter inside the
// window (on a phone the clock sits near the right edge).
export function PatrolHiddenHint({ onUndo }: { onUndo?: () => void }) {
  const ref = useRef<HTMLDivElement | null>(null);
  const [shift, setShift] = useState(0);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el || typeof window === "undefined") return;
    const rect = el.getBoundingClientRect();
    const gutter = 16;
    if (rect.right > window.innerWidth - gutter) setShift(window.innerWidth - gutter - rect.right);
    else if (rect.left < gutter) setShift(gutter - rect.left);
  }, []);
  return (
    <div
      ref={ref}
      role="status"
      data-testid="patrol-hidden-hint"
      style={shift ? { marginLeft: shift } : undefined}
      className="absolute bottom-[calc(100%+0.75rem)] left-1/2 z-[600] flex w-[min(18.75rem,calc(100vw-2rem))] -translate-x-1/2 items-center gap-3 rounded-[var(--radius-lg)] border border-[var(--color-selected-border)] bg-[var(--color-surface-2)] py-3 pr-3 pl-3.5 shadow-[var(--shadow-popover)]"
    >
      <span className="grid min-w-0 flex-1 gap-0.5">
        <span className="text-[0.8125rem] font-semibold text-[var(--color-text-primary)]">
          {tr("Patrol bar hidden")}
        </span>
        <span className="text-[0.75rem] leading-[1.4] text-[var(--color-text-secondary)]">
          {tr("Still running. Manage it from this clock anytime.")}
        </span>
      </span>
      {onUndo ? (
        <button
          type="button"
          data-testid="patrol-hidden-undo"
          onClick={onUndo}
          className="pill-btn h-8 shrink-0 px-3 font-medium"
        >
          {tr("Undo")}
        </button>
      ) : null}
    </div>
  );
}
