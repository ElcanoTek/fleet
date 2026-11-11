import { rich } from "@/app/lib/i18n/rich";
import { t as tr } from "@/app/lib/i18n";
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
        const countdown = !isPaused ? formatCountdown(sc.next_run_at, nowSec) : tr("paused");
        const runsLabel =
          (sc.max_runs ?? 0) > 0
            ? tr("Run {0}/{1}", [sc.run_count, sc.max_runs])
            : sc.run_count > 0
              ? tr("Run #{0}", [sc.run_count])
              : tr("Ready");
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
                  {sc.kind === "timer" ? tr("timer") : tr("patrol")}
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
                // A switch, not a status pill: "Auto-approve off" in a plain
                // pill read as the current state, not as the control that turns
                // it on (a Raptive trader, 2026-11-10). The label stays fixed;
                // the knob and the accent tint show the state.
                <button
                  type="button"
                  role="switch"
                  aria-checked={Boolean(isAutoApprove)}
                  data-testid="patrol-auto-approve"
                  onClick={onToggleAutoApprove}
                  title={
                    isAutoApprove
                      ? tr("Auto-approve is on: patrol runs act without approval cards. Click to turn it off.")
                      : tr("Auto-approve is off: patrol runs wait on approval cards. Click to turn it on.")
                  }
                  className={`pill-btn inline-flex items-center gap-1.5 ${
                    isAutoApprove
                      ? "border-[color-mix(in_srgb,var(--color-accent)_45%,var(--color-border-strong))] bg-[color-mix(in_srgb,var(--color-accent)_16%,transparent)] text-[var(--color-accent)]"
                      : ""
                  }`}
                >
                  <Icon name="zap" className="size-3" />
                  <span>{tr("Auto-approve")}</span>
                  <span
                    aria-hidden="true"
                    className={`relative inline-block h-[0.875rem] w-[1.5rem] shrink-0 rounded-full transition ${
                      isAutoApprove
                        ? "bg-[var(--color-switch-track-on)]"
                        : "bg-[var(--color-switch-track-off)]"
                    }`}
                  >
                    <span
                      className={`absolute left-[2px] top-[2px] size-[0.625rem] rounded-full shadow-[var(--shadow-switch-knob)] transition-transform ${
                        isAutoApprove
                          ? "translate-x-[0.625rem] bg-[var(--color-switch-knob-on)]"
                          : "bg-[var(--color-switch-knob)]"
                      }`}
                    />
                  </span>
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
                {tr("Run now")}
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
                {isPaused ? tr("Resume") : tr("Pause")}
              </button>
              <button
                type="button"
                disabled={busy}
                aria-label={tr("Stop {0}", [sc.title])}
                title={tr("Cancel patrol")}
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
  { id: "1h", label: tr("Every 1h"), kind: "patrol" as const, cron: "0 * * * *", durationSeconds: 0 },
  { id: "2h", label: tr("Every 2h"), kind: "patrol" as const, cron: "0 */2 * * *", durationSeconds: 0 },
  { id: "4h", label: tr("Every 4h"), kind: "patrol" as const, cron: "0 */4 * * *", durationSeconds: 0 },
  { id: "6h", label: tr("Every 6h"), kind: "patrol" as const, cron: "0 */6 * * *", durationSeconds: 0 },
  { id: "weekdays", label: tr("Weekdays 9am"), kind: "patrol" as const, cron: "0 9 * * 1-5", durationSeconds: 0 },
  { id: "daily", label: tr("Daily 9am"), kind: "patrol" as const, cron: "0 9 * * *", durationSeconds: 0 },
  { id: "timer30m", label: tr("In 30m (once)"), kind: "timer" as const, cron: "", durationSeconds: 1800 },
  { id: "timer2h", label: tr("In 2h (once)"), kind: "timer" as const, cron: "", durationSeconds: 7200 },
  { id: "custom", label: tr("Custom"), kind: "patrol" as const, cron: "0 9 * * 1-5", durationSeconds: 0 },
];

const CUSTOM_TIMER_PRESETS = [
  { label: tr("15m"), value: 15, unit: "minutes" as const },
  { label: tr("45m"), value: 45, unit: "minutes" as const },
  { label: tr("1h"), value: 1, unit: "hours" as const },
  { label: tr("3h"), value: 3, unit: "hours" as const },
  { label: tr("4h"), value: 4, unit: "hours" as const },
  { label: tr("8h"), value: 8, unit: "hours" as const },
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
  const [title, setTitle] = useState<string>(tr("Recurring Patrol"));
  const [patrolPrompt, setPatrolPrompt] = useState<string>(() =>
    (initialPrompt ?? "").trim() ||
    tr("Check for new updates, verify key metrics, and summarize anything that needs attention."),
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
        aria-label={tr("Configure in-chat patrol")}
        data-testid="patrol-popover"
        className="picker-modal relative z-10 outline-none"
      >
        <header className="picker-modal__head">
          <span aria-hidden="true" className="picker-icon-tile shrink-0">
            <Icon name="clock" className="size-4" />
          </span>
          <div className="min-w-0 flex-1">
            <h2 className="picker-modal__title">{tr("In-Chat Patrol & Timer")}</h2>
            <p className="m-0 text-[0.75rem] leading-[1.35] text-[var(--color-text-subtle)]">
              {tr("Wakes this conversation on your cadence with full history, workspace files, and enabled connectors.")}
            </p>
          </div>
          <CloseButton
            label={tr("Close patrol setup")}
            onClick={onClose}
            className="any-pointer-coarse:size-11"
          />
        </header>

        <div className="picker-modal__rail">
          <div className="font-[family-name:var(--font-code)] text-[0.6875rem] uppercase tracking-[0.12em] text-[var(--color-text-subtle)]">
            {tr("Cadence")}
          </div>
          <div
            className="flex flex-wrap gap-1.5"
            role="group"
            aria-label={tr("Patrol cadence presets")}
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
                aria-label={tr("Schedule mode")}
              >
                <button
                  type="button"
                  aria-pressed={customKind === "patrol"}
                  onClick={() => setCustomKind("patrol")}
                  className="seg-option flex-1 justify-center"
                >
                  {tr("Recurring schedule")}
                </button>
                <button
                  type="button"
                  aria-pressed={customKind === "timer"}
                  onClick={() => setCustomKind("timer")}
                  className="seg-option flex-1 justify-center"
                >
                  {tr("One-time timer")}
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
        {rich("In {0}", tp.label)}
      </button>
                      );
                    })}
                  </div>
                  <div className="grid grid-cols-2 gap-2">
                    <label className="grid gap-1 text-[0.72rem] font-medium text-[var(--color-text-secondary)]">
                      <span>{tr("Run once in")}</span>
                      <input
                        type="number"
                        min={1}
                        max={maxTimerAmount}
                        aria-label={tr("Timer duration")}
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
                      <span>{tr("Unit")}</span>
                      <select
                        aria-label={tr("Timer unit")}
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
                        <option value="minutes">{tr("Minutes")}</option>
                        <option value="hours">{tr("Hours")}</option>
                      </select>
                    </label>
                  </div>
                </div>
              )}
            </div>
          ) : null}

          <label className="grid gap-1">
            <span className="font-[family-name:var(--font-code)] text-[0.6875rem] uppercase tracking-[0.12em] text-[var(--color-text-subtle)]">
              {tr("Patrol label")}
            </span>
            <input
              type="text"
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              placeholder={tr("Recurring Patrol")}
              className="h-9 w-full rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] px-3 text-[0.8125rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
            />
          </label>

          <label className="grid gap-1">
            <span className="font-[family-name:var(--font-code)] text-[0.6875rem] uppercase tracking-[0.12em] text-[var(--color-text-subtle)]">
              {tr("What to check & do each run")}
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
                <span>{tr("Auto-approve tools while patrolling")}</span>
              </div>
              <div className="picker-row__desc">
                {tr("Skip interactive approval cards during unattended patrol runs.")}
              </div>
            </div>
            <button
              type="button"
              role="switch"
              aria-checked={autoApprove}
              aria-label={tr("Auto-approve tools while patrolling")}
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
              ? tr("Runs once in this conversation")
              : tr("Recurring schedule ({0})", [tz])}
          </span>
          <div className="ml-auto flex items-center gap-2">
            <button
              type="button"
              onClick={onClose}
              className="btn btn-ghost btn-sm"
            >
              {tr("Cancel")}</button>
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
                    title: title.trim() || tr("Chat Patrol"),
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
                ? tr("Starting…")
                : effectiveKind === "timer"
                  ? tr("Start timer")
                  : tr("Start patrol")}
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
