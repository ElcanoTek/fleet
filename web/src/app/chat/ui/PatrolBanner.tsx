import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Icon } from "./Icon";
import type { ChatSchedule } from "./history";
import { CloseButton } from "@/app/shared/ui/CloseButton";
import { StatusChip } from "@/app/shared/ui/StatusChip";
import { useDialogA11y } from "@/app/shared/ui/useDialogA11y";
import {
  describeCronExpression,
  describeCronExpressionShort,
} from "@/app/shared/lib/cron";
import { browserTimeZone } from "@/app/shared/lib/timezones";
import { PatrolSchedulePicker } from "@/app/shared/ui/PatrolSchedulePicker";
import { openedByTouch } from "./toolsPicker";

export function formatScheduleCadence(sc: ChatSchedule): string {
  if (sc.kind === "timer") {
    const secs = sc.duration_seconds ?? 0;
    if (secs >= 3600 && secs % 3600 === 0) return `One-shot (${secs / 3600}h)`;
    if (secs >= 60) return `One-shot (${Math.round(secs / 60)}m)`;
    if (secs > 0) return `One-shot (${secs}s)`;
    return "One-shot timer";
  }
  const cron = (sc.cron ?? sc.cron_expr ?? "").trim();
  if (!cron) return "Recurring patrol";
  if (cron === "0 * * * *" || cron === "0 */1 * * *") return "Every hour";
  const stepHours = cron.match(/^0 \*\/(\d+) \* \* \*$/);
  if (stepHours) return `Every ${stepHours[1]}h`;
  const stepMins = cron.match(/^\*\/(\d+) \* \* \* \*$/);
  if (stepMins) return `Every ${stepMins[1]}m`;
  const daily = cron.match(/^(\d+) (\d+) \* \* \*$/);
  if (daily) {
    const hh = daily[2].padStart(2, "0");
    const mm = daily[1].padStart(2, "0");
    return `Daily at ${hh}:${mm}`;
  }
  const weekdays = cron.match(/^(\d+) (\d+) \* \* (?:1-5|MON-FRI)$/i);
  if (weekdays) {
    const hh = weekdays[2].padStart(2, "0");
    const mm = weekdays[1].padStart(2, "0");
    return `Weekdays at ${hh}:${mm}`;
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
  if (diff <= 0) return "due now";
  if (diff < 60) return `in ${diff}s`;
  const mins = Math.floor(diff / 60);
  if (mins < 60) return `in ${mins}m`;
  const hours = Math.floor(mins / 60);
  const remMins = mins % 60;
  if (hours < 24) return remMins > 0 ? `in ${hours}h ${remMins}m` : `in ${hours}h`;
  const days = Math.floor(hours / 24);
  return `in ${days}d ${hours % 24}h`;
}

export function PatrolBanner({
  schedules,
  isAutoApprove,
  isStreaming,
  onToggleAutoApprove,
  onRunNow,
  onTogglePause,
  onCancel,
}: {
  schedules: ChatSchedule[];
  isAutoApprove?: boolean;
  isStreaming?: boolean;
  onToggleAutoApprove?: () => void;
  onRunNow: (scheduleId: string) => void | Promise<void>;
  onTogglePause: (scheduleId: string, currentStatus: ChatSchedule["status"]) => void | Promise<void>;
  onCancel: (scheduleId: string) => void | Promise<void>;
}) {
  const visible = schedules.filter((s) => s.status === "active" || s.status === "paused");
  const [nowSec, setNowSec] = useState<number>(() => Math.floor(Date.now() / 1000));
  const [busyId, setBusyId] = useState<string | null>(null);

  useEffect(() => {
    if (visible.length === 0) return;
    const id = window.setInterval(() => {
      setNowSec(Math.floor(Date.now() / 1000));
    }, 10_000);
    return () => window.clearInterval(id);
  }, [visible.length]);

  if (visible.length === 0) return null;

  return (
    <div
      className="mx-auto mb-1.5 flex w-full max-w-[53rem] flex-col gap-1.5 px-1 sm:px-0"
      data-testid="patrol-banner"
    >
      {visible.map((sc) => {
        const isPaused = sc.status === "paused";
        const cadence = formatScheduleCadence(sc);
        const countdown = !isPaused ? formatCountdown(sc.next_run_at, nowSec) : "paused";
        const runsLabel =
          (sc.max_runs ?? 0) > 0
            ? `Run ${sc.run_count}/${sc.max_runs}`
            : sc.run_count > 0
              ? `Run #${sc.run_count}`
              : "Ready";
        const busy = busyId === sc.id || Boolean(isStreaming);
        return (
          <div
            key={sc.id}
            className="flex flex-wrap items-center justify-between gap-x-2 gap-y-1.5 rounded-[var(--radius-lg)] border border-[color-mix(in_srgb,var(--color-accent)_34%,var(--color-border))] bg-[color-mix(in_srgb,var(--color-accent)_6%,var(--color-surface-2))] px-2.5 py-1.5 text-xs text-[var(--color-text-secondary)] shadow-xs sm:px-3"
          >
            <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-2 gap-y-0.5">
              <div className="flex min-w-0 items-center gap-1.5">
                <span
                  className={`inline-block size-2 shrink-0 rounded-full ${
                    isPaused
                      ? "bg-[var(--color-warning,#f59e0b)]"
                      : "animate-pulse bg-[var(--color-status-success-fg,#10b981)]"
                  }`}
                />
                <StatusChip tone={isPaused ? "neutral" : "accent"} caps className="shrink-0">
                  {sc.kind === "timer" ? "timer" : "patrol"}
                </StatusChip>
                <span
                  className="min-w-0 truncate font-medium text-[var(--color-text-primary)]"
                  title={sc.prompt}
                >
                  {sc.title}
                </span>
              </div>
              <span className="text-[0.72rem] text-[var(--color-text-muted)]">
                · {cadence}
                {countdown ? ` (${countdown})` : ""} · {runsLabel}
              </span>
            </div>

            <div className="flex shrink-0 flex-wrap items-center gap-1.5">
              {onToggleAutoApprove ? (
                <button
                  type="button"
                  aria-pressed={Boolean(isAutoApprove)}
                  onClick={onToggleAutoApprove}
                  title={
                    isAutoApprove
                      ? "Auto-approve is ON for this patrol chat (click to turn off)"
                      : "Enable Auto-approve so unattended patrol runs don't pause on approval cards"
                  }
                  className="pill-btn inline-flex items-center gap-1"
                >
                  <Icon name="zap" className="size-3" />
                  <span>{isAutoApprove ? "Auto-approve on" : "Auto-approve off"}</span>
                </button>
              ) : null}
              <button
                type="button"
                disabled={busy}
                onClick={async () => {
                  setBusyId(sc.id);
                  try {
                    await onRunNow(sc.id);
                  } finally {
                    setBusyId(null);
                  }
                }}
                className="pill-btn font-medium disabled:opacity-40"
              >
                Run now
              </button>
              <button
                type="button"
                disabled={busy}
                onClick={async () => {
                  setBusyId(sc.id);
                  try {
                    await onTogglePause(sc.id, sc.status);
                  } finally {
                    setBusyId(null);
                  }
                }}
                className="pill-btn disabled:opacity-40"
              >
                {isPaused ? "Resume" : "Pause"}
              </button>
              <button
                type="button"
                disabled={busy}
                aria-label={`Stop ${sc.title}`}
                title="Cancel patrol"
                onClick={async () => {
                  setBusyId(sc.id);
                  try {
                    await onCancel(sc.id);
                  } finally {
                    setBusyId(null);
                  }
                }}
                className="inline-flex size-[22px] items-center justify-center rounded-[var(--radius-pill)] border border-[var(--color-pill-border)] text-[var(--color-text-muted)] transition hover:border-[var(--color-status-error-border)] hover:bg-[var(--color-status-error-bg)] hover:text-[var(--color-status-error-fg)] disabled:opacity-40"
              >
                <Icon name="close" className="size-3" />
              </button>
            </div>
          </div>
        );
      })}
    </div>
  );
}

const PATROL_PRESETS = [
  { id: "1h", label: "Every 1h", kind: "patrol" as const, cron: "0 * * * *", durationSeconds: 0 },
  { id: "2h", label: "Every 2h", kind: "patrol" as const, cron: "0 */2 * * *", durationSeconds: 0 },
  { id: "4h", label: "Every 4h", kind: "patrol" as const, cron: "0 */4 * * *", durationSeconds: 0 },
  { id: "6h", label: "Every 6h", kind: "patrol" as const, cron: "0 */6 * * *", durationSeconds: 0 },
  { id: "weekdays", label: "Weekdays 9am", kind: "patrol" as const, cron: "0 9 * * 1-5", durationSeconds: 0 },
  { id: "daily", label: "Daily 9am", kind: "patrol" as const, cron: "0 9 * * *", durationSeconds: 0 },
  { id: "timer30m", label: "In 30m (once)", kind: "timer" as const, cron: "", durationSeconds: 1800 },
  { id: "timer2h", label: "In 2h (once)", kind: "timer" as const, cron: "", durationSeconds: 7200 },
  { id: "custom", label: "Custom", kind: "patrol" as const, cron: "0 9 * * 1-5", durationSeconds: 0 },
];

const CUSTOM_TIMER_PRESETS = [
  { label: "15m", value: 15, unit: "minutes" as const },
  { label: "45m", value: 45, unit: "minutes" as const },
  { label: "1h", value: 1, unit: "hours" as const },
  { label: "3h", value: 3, unit: "hours" as const },
  { label: "4h", value: 4, unit: "hours" as const },
  { label: "8h", value: 8, unit: "hours" as const },
];

export type CreatePatrolInput = {
  title: string;
  prompt: string;
  cron?: string;
  timezone?: string;
  durationSeconds?: number;
  autoApprove: boolean;
};

export function PatrolPopover({
  initialPrompt,
  onStartPatrol,
  onClose,
}: {
  initialPrompt?: string;
  onStartPatrol: (input: CreatePatrolInput) => boolean | void | Promise<boolean | void>;
  onClose: () => void;
}) {
  const [presetId, setPresetId] = useState<string>("2h");
  const [customKind, setCustomKind] = useState<"patrol" | "timer">("patrol");
  const [customCron, setCustomCron] = useState<string>("0 9 * * 1-5");
  const [customTz, setCustomTz] = useState<string>(() => browserTimeZone());
  const [customTimerAmount, setCustomTimerAmount] = useState<number>(45);
  const [customTimerUnit, setCustomTimerUnit] = useState<"minutes" | "hours">("minutes");
  const [title, setTitle] = useState<string>("Recurring Patrol");
  const [patrolPrompt, setPatrolPrompt] = useState<string>(() =>
    (initialPrompt ?? "").trim() ||
    "Check for new updates, verify key metrics, and summarize anything that needs attention.",
  );
  const [autoApprove, setAutoApprove] = useState<boolean>(true);
  const [submitting, setSubmitting] = useState<boolean>(false);
  const panelRef = useRef<HTMLDivElement | null>(null);
  const promptInputRef = useRef<HTMLTextAreaElement | null>(null);

  useDialogA11y(true, panelRef, onClose, {
    initialFocusRef: openedByTouch() ? panelRef : promptInputRef,
  });

  const selectedPreset = PATROL_PRESETS.find((p) => p.id === presetId) ?? PATROL_PRESETS[1];
  const isCustom = selectedPreset.id === "custom";
  const effectiveKind: "patrol" | "timer" = isCustom ? customKind : selectedPreset.kind;
  const effectiveCron =
    effectiveKind === "patrol"
      ? isCustom
        ? customCron.trim()
        : selectedPreset.cron
      : undefined;
  const maxTimerAmount = customTimerUnit === "hours" ? 168 : 1440;
  const clampedTimerAmount = Math.min(
    maxTimerAmount,
    Math.max(1, Math.round(customTimerAmount || 1)),
  );
  const customDurationSeconds =
    customTimerUnit === "hours"
      ? clampedTimerAmount * 3600
      : clampedTimerAmount * 60;
  const effectiveDurationSeconds =
    effectiveKind === "timer"
      ? isCustom
        ? customDurationSeconds
        : selectedPreset.durationSeconds
      : undefined;
  const tz =
    effectiveKind === "patrol" && isCustom
      ? customTz.trim() || "UTC"
      : browserTimeZone();

  const modalContent = (
    <div className="scrim fixed inset-0 z-[1200] flex items-stretch justify-center p-0 sm:items-center sm:p-4">
      <button
        type="button"
        tabIndex={-1}
        aria-hidden="true"
        onClick={onClose}
        className="absolute inset-0 cursor-default"
      />
      <div
        ref={panelRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-label="Configure in-chat patrol"
        data-testid="patrol-popover"
        className="picker-modal relative z-10 outline-none"
      >
        <header className="picker-modal__head">
          <span aria-hidden="true" className="picker-icon-tile shrink-0">
            <Icon name="clock" className="size-4" />
          </span>
          <div className="min-w-0 flex-1">
            <h2 className="picker-modal__title">In-Chat Patrol &amp; Timer</h2>
            <p className="m-0 text-[0.75rem] leading-[1.35] text-[var(--color-text-subtle)]">
              Wakes this conversation on your cadence with full history, workspace files, and enabled connectors.
            </p>
          </div>
          <CloseButton
            label="Close patrol setup"
            onClick={onClose}
            className="any-pointer-coarse:size-11"
          />
        </header>

        <div className="picker-modal__rail">
          <div className="font-[family-name:var(--font-code)] text-[0.6875rem] uppercase tracking-[0.12em] text-[var(--color-text-subtle)]">
            Cadence
          </div>
          <div
            className="flex flex-wrap gap-1.5"
            role="group"
            aria-label="Patrol cadence presets"
          >
            {PATROL_PRESETS.map((preset) => {
              const active = preset.id === presetId;
              return (
                <button
                  key={preset.id}
                  type="button"
                  aria-pressed={active}
                  onClick={() => setPresetId(preset.id)}
                  className="pill-btn hit-area-touch h-7 px-3 text-[0.75rem]"
                >
                  {preset.label}
                </button>
              );
            })}
          </div>
        </div>

        <div className="picker-modal__body grid content-start gap-3">
          {isCustom ? (
            <div className="grid gap-2.5" data-testid="patrol-custom-builder">
              <div
                className="seg w-full"
                role="group"
                aria-label="Schedule mode"
              >
                <button
                  type="button"
                  aria-pressed={customKind === "patrol"}
                  onClick={() => setCustomKind("patrol")}
                  className="seg-option flex-1 justify-center"
                >
                  Recurring schedule
                </button>
                <button
                  type="button"
                  aria-pressed={customKind === "timer"}
                  onClick={() => setCustomKind("timer")}
                  className="seg-option flex-1 justify-center"
                >
                  One-time timer
                </button>
              </div>

              {customKind === "patrol" ? (
                <PatrolSchedulePicker
                  compact
                  cron={customCron}
                  timezone={customTz}
                  onChangeCron={setCustomCron}
                  onChangeTimezone={setCustomTz}
                  testIdPrefix="patrol-popover-schedule"
                />
              ) : (
                <div
                  data-testid="patrol-custom-timer"
                  className="grid gap-2.5 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-inset)] p-3"
                >
                  <div className="flex flex-wrap gap-1.5">
                    {CUSTOM_TIMER_PRESETS.map((tp) => {
                      const active =
                        customTimerAmount === tp.value &&
                        customTimerUnit === tp.unit;
                      return (
                        <button
                          key={tp.label}
                          type="button"
                          aria-pressed={active}
                          onClick={() => {
                            setCustomTimerAmount(tp.value);
                            setCustomTimerUnit(tp.unit);
                          }}
                          className="pill-btn hit-area-touch h-6 px-2.5 text-[0.72rem]"
                        >
                          In {tp.label}
                        </button>
                      );
                    })}
                  </div>
                  <div className="grid grid-cols-2 gap-2">
                    <label className="grid gap-1 text-[0.72rem] font-medium text-[var(--color-text-secondary)]">
                      <span>Run once in</span>
                      <input
                        type="number"
                        min={1}
                        max={maxTimerAmount}
                        aria-label="Timer duration"
                        value={customTimerAmount}
                        onChange={(e) =>
                          setCustomTimerAmount(
                            Math.min(
                              maxTimerAmount,
                              Math.max(1, Math.round(Number(e.target.value) || 1)),
                            ),
                          )
                        }
                        className="h-9 w-full rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-2.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                      />
                    </label>
                    <label className="grid gap-1 text-[0.72rem] font-medium text-[var(--color-text-secondary)]">
                      <span>Unit</span>
                      <select
                        aria-label="Timer unit"
                        value={customTimerUnit}
                        onChange={(e) => {
                          const nextUnit = e.target.value as "minutes" | "hours";
                          const nextMax = nextUnit === "hours" ? 168 : 1440;
                          setCustomTimerUnit(nextUnit);
                          setCustomTimerAmount((prev) =>
                            Math.min(nextMax, Math.max(1, Math.round(prev || 1))),
                          );
                        }}
                        className="h-9 w-full rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-2.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
                      >
                        <option value="minutes">Minutes</option>
                        <option value="hours">Hours</option>
                      </select>
                    </label>
                  </div>
                </div>
              )}
            </div>
          ) : null}

          <label className="grid gap-1">
            <span className="font-[family-name:var(--font-code)] text-[0.6875rem] uppercase tracking-[0.12em] text-[var(--color-text-subtle)]">
              Patrol label
            </span>
            <input
              type="text"
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              placeholder="Recurring Patrol"
              className="h-9 w-full rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] px-3 text-[0.8125rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
            />
          </label>

          <label className="grid gap-1">
            <span className="font-[family-name:var(--font-code)] text-[0.6875rem] uppercase tracking-[0.12em] text-[var(--color-text-subtle)]">
              What to check &amp; do each run
            </span>
            <textarea
              ref={promptInputRef}
              rows={3}
              value={patrolPrompt}
              onChange={(e) => setPatrolPrompt(e.target.value)}
              className="w-full resize-y rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] px-3 py-2 text-[0.8125rem] leading-[1.5] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
            />
          </label>

          <div className="picker-row picker-row--active m-0 items-center justify-between">
            <div className="picker-row__body">
              <div className="picker-row__name">
                <Icon name="zap" className="size-3.5 text-[var(--color-accent)]" />
                <span>Auto-approve tools while patrolling</span>
              </div>
              <div className="picker-row__desc">
                Skip interactive approval cards during unattended patrol runs.
              </div>
            </div>
            <button
              type="button"
              role="switch"
              aria-checked={autoApprove}
              aria-label="Auto-approve tools while patrolling"
              onClick={() => setAutoApprove((v) => !v)}
              className="switch"
            >
              <span className="switch__knob" />
            </button>
          </div>
        </div>

        <footer className="picker-modal__foot">
          <span className="min-w-0 truncate text-[0.75rem] text-[var(--color-text-subtle)]">
            {effectiveKind === "timer"
              ? "Runs once in this conversation"
              : `Recurring schedule (${tz})`}
          </span>
          <div className="ml-auto flex items-center gap-2">
            <button
              type="button"
              onClick={onClose}
              className="btn btn-ghost btn-sm"
            >
              Cancel
            </button>
            <button
              type="button"
              disabled={
                !patrolPrompt.trim() ||
                (effectiveKind === "patrol" && !effectiveCron) ||
                submitting
              }
              onClick={async () => {
                setSubmitting(true);
                try {
                  const ok = await onStartPatrol({
                    title: title.trim() || "Chat Patrol",
                    prompt: patrolPrompt.trim(),
                    cron: effectiveCron,
                    timezone: tz,
                    durationSeconds: effectiveDurationSeconds,
                    autoApprove,
                  });
                  if (ok !== false) {
                    onClose();
                  }
                } finally {
                  setSubmitting(false);
                }
              }}
              className="btn btn-primary btn-sm"
            >
              {submitting
                ? "Starting…"
                : effectiveKind === "timer"
                  ? "Start timer"
                  : "Start patrol"}
            </button>
          </div>
        </footer>
      </div>
    </div>
  );

  if (typeof document === "undefined") {
    return modalContent;
  }
  return createPortal(modalContent, document.body);
}
