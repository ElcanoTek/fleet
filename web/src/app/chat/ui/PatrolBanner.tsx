"use client";

import { useEffect, useState } from "react";
import { Icon } from "./Icon";
import type { ChatSchedule } from "./history";
import { StatusChip } from "@/app/shared/ui/StatusChip";

export function formatScheduleCadence(sc: ChatSchedule): string {
  if (sc.kind === "timer") {
    const secs = sc.duration_seconds ?? 0;
    if (secs >= 3600 && secs % 3600 === 0) return `One-shot (${secs / 3600}h)`;
    if (secs >= 60) return `One-shot (${Math.round(secs / 60)}m)`;
    if (secs > 0) return `One-shot (${secs}s)`;
    return "One-shot timer";
  }
  const cron = (sc.cron ?? sc.cron_expr ?? "").trim();
  if (cron === "0 * * * *") return "Every hour";
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
  return cron || "Recurring patrol";
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
            className="flex flex-wrap items-center justify-between gap-2 rounded-[var(--radius-lg)] border border-[color-mix(in_srgb,var(--color-accent)_34%,var(--color-border))] bg-[color-mix(in_srgb,var(--color-accent)_6%,var(--color-surface-2))] px-3 py-1.5 text-xs text-[var(--color-text-secondary)] shadow-xs"
          >
            <div className="flex min-w-0 flex-1 items-center gap-2">
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
              <span className="shrink-0 text-[0.72rem] text-[var(--color-text-muted)]">
                · {cadence}
                {countdown ? ` (${countdown})` : ""} · {runsLabel}
              </span>
            </div>

            <div className="flex shrink-0 items-center gap-1.5">
              {onToggleAutoApprove ? (
                <button
                  type="button"
                  onClick={onToggleAutoApprove}
                  title={
                    isAutoApprove
                      ? "Auto-approve is ON for this patrol chat (click to turn off)"
                      : "Enable Auto-approve so unattended patrol runs don't pause on approval cards"
                  }
                  className={`inline-flex items-center gap-1 rounded-[var(--radius-pill)] border px-2 py-0.5 text-[0.68rem] font-medium transition ${
                    isAutoApprove
                      ? "border-[color-mix(in_srgb,var(--color-accent)_45%,transparent)] bg-[color-mix(in_srgb,var(--color-accent)_14%,transparent)] text-[var(--color-accent)]"
                      : "border-[var(--color-border)] text-[var(--color-text-muted)] hover:border-[var(--color-border-strong)] hover:text-[var(--color-text-primary)]"
                  }`}
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
                className="rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-2 py-0.5 text-[0.7rem] font-medium text-[var(--color-text-primary)] transition hover:border-[var(--color-accent)] hover:text-[var(--color-accent)] disabled:opacity-40"
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
                className="rounded-[var(--radius-md)] border border-[var(--color-border)] px-2 py-0.5 text-[0.7rem] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] disabled:opacity-40"
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
                className="rounded-[var(--radius-md)] p-1 text-[var(--color-text-muted)] transition hover:bg-[var(--color-status-error-bg)] hover:text-[var(--color-status-error-fg)] disabled:opacity-40"
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
  { id: "daily", label: "Daily 9am", kind: "patrol" as const, cron: "0 9 * * *", durationSeconds: 0 },
  { id: "timer30m", label: "In 30m (once)", kind: "timer" as const, cron: "", durationSeconds: 1800 },
  { id: "timer2h", label: "In 2h (once)", kind: "timer" as const, cron: "", durationSeconds: 7200 },
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
  onStartPatrol: (input: CreatePatrolInput) => void | Promise<void>;
  onClose: () => void;
}) {
  const [presetId, setPresetId] = useState<string>("2h");
  const [title, setTitle] = useState<string>("Trading Patrol");
  const [patrolPrompt, setPatrolPrompt] = useState<string>(() =>
    (initialPrompt ?? "").trim() ||
    "Patrol active campaigns, deal pacing, and recent inbox/report updates. Flag any anomalies, under-delivery, or margin shifts and take or stage the needed updates.",
  );
  const [autoApprove, setAutoApprove] = useState<boolean>(true);
  const [submitting, setSubmitting] = useState<boolean>(false);

  const selectedPreset = PATROL_PRESETS.find((p) => p.id === presetId) ?? PATROL_PRESETS[1];
  const tz =
    typeof Intl !== "undefined"
      ? Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC"
      : "UTC";

  return (
    <div
      role="dialog"
      aria-label="Configure in-chat patrol"
      data-testid="patrol-popover"
      className="fixed inset-x-2 bottom-[calc(env(safe-area-inset-bottom,0px)+5rem)] z-30 flex flex-col gap-2.5 rounded-[0.85rem] border border-[var(--color-border-strong)] bg-[var(--color-surface-2)] p-3.5 shadow-[var(--shadow-md)] motion-safe:animate-pop-up sm:absolute sm:inset-x-auto sm:bottom-[calc(100%+0.55rem)] sm:left-0 sm:w-[22rem]"
    >
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-1.5">
          <Icon name="clock" className="size-3.5 text-[var(--color-accent)]" />
          <span className="text-[0.82rem] font-semibold text-[var(--color-text-primary)]">
            In-Chat Patrol & Timer
          </span>
        </div>
        <button
          type="button"
          aria-label="Close patrol setup"
          onClick={onClose}
          className="rounded p-1 text-[var(--color-text-muted)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]"
        >
          <Icon name="close" className="size-3.5" />
        </button>
      </div>

      <p className="text-[0.72rem] leading-[1.45] text-[var(--color-text-muted)]">
        Wakes this conversation on your cadence with full memory and persistent Python state.
      </p>

      <div className="flex flex-wrap gap-1">
        {PATROL_PRESETS.map((preset) => {
          const active = preset.id === presetId;
          return (
            <button
              key={preset.id}
              type="button"
              onClick={() => setPresetId(preset.id)}
              className={`rounded-[var(--radius-pill)] border px-2 py-0.5 text-[0.7rem] font-medium transition ${
                active
                  ? "border-[var(--color-accent)] bg-[color-mix(in_srgb,var(--color-accent)_16%,transparent)] text-[var(--color-accent)]"
                  : "border-[var(--color-border)] text-[var(--color-text-secondary)] hover:border-[var(--color-border-strong)]"
              }`}
            >
              {preset.label}
            </button>
          );
        })}
      </div>

      <label className="grid gap-1">
        <span className="text-[0.68rem] font-medium uppercase tracking-wider text-[var(--color-text-muted)]">
          Patrol label
        </span>
        <input
          type="text"
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          placeholder="Trading Patrol"
          className="w-full rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-2.5 py-1.5 text-[0.78rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
        />
      </label>

      <label className="grid gap-1">
        <span className="text-[0.68rem] font-medium uppercase tracking-wider text-[var(--color-text-muted)]">
          What to check & do each run
        </span>
        <textarea
          rows={3}
          value={patrolPrompt}
          onChange={(e) => setPatrolPrompt(e.target.value)}
          className="w-full resize-y rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-2.5 py-1.5 text-[0.76rem] leading-[1.45] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
        />
      </label>

      <label className="flex cursor-pointer items-center justify-between gap-2 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-2.5 py-1.5 text-[0.74rem] text-[var(--color-text-secondary)]">
        <span className="flex items-center gap-1.5">
          <Icon name="zap" className="size-3.5 text-[var(--color-accent)]" />
          <span>Auto-approve tools while patrolling</span>
        </span>
        <input
          type="checkbox"
          checked={autoApprove}
          onChange={(e) => setAutoApprove(e.target.checked)}
          className="accent-[var(--color-accent)]"
        />
      </label>

      <div className="flex items-center justify-end gap-2 pt-0.5">
        <button
          type="button"
          onClick={onClose}
          className="rounded-[var(--radius-md)] px-2.5 py-1 text-[0.75rem] text-[var(--color-text-muted)] hover:text-[var(--color-text-primary)]"
        >
          Cancel
        </button>
        <button
          type="button"
          disabled={!patrolPrompt.trim() || submitting}
          onClick={async () => {
            setSubmitting(true);
            try {
              await onStartPatrol({
                title: title.trim() || "Chat Patrol",
                prompt: patrolPrompt.trim(),
                cron: selectedPreset.kind === "patrol" ? selectedPreset.cron : undefined,
                timezone: tz,
                durationSeconds:
                  selectedPreset.kind === "timer" ? selectedPreset.durationSeconds : undefined,
                autoApprove,
              });
              onClose();
            } finally {
              setSubmitting(false);
            }
          }}
          className="rounded-[var(--radius-md)] bg-[image:var(--gradient-action-fill)] px-3 py-1 text-[0.76rem] font-medium text-[var(--color-on-primary-fill)] transition enabled:hover:bg-[image:var(--gradient-action-fill-hover)] disabled:opacity-40"
        >
          {submitting
            ? "Starting…"
            : selectedPreset.kind === "timer"
              ? "Start timer"
              : "Start patrol"}
        </button>
      </div>
    </div>
  );
}
