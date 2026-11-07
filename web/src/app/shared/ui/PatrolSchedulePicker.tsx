import { useMemo, useState } from "react";
import { describeCronExpression } from "@/app/shared/lib/cron";
import {
  buildPatrolCron,
  HOUR_INTERVAL_OPTIONS,
  MINUTE_INTERVAL_OPTIONS,
  parsePatrolCron,
  PATROL_WEEKDAYS,
  sortPatrolDays,
  type PatrolFrequency,
  type PatrolScheduleState,
} from "@/app/shared/lib/patrolSchedule";
import {
  browserTimeZone,
  formatTimeZoneLabel,
  timeZoneOptions,
} from "@/app/shared/lib/timezones";

type PatrolSchedulePickerProps = {
  cron: string;
  timezone: string;
  onChangeCron: (nextCron: string) => void;
  onChangeTimezone?: (nextTimezone: string) => void;
  compact?: boolean;
  testIdPrefix?: string;
};

export function PatrolSchedulePicker({
  cron,
  timezone,
  onChangeCron,
  onChangeTimezone,
  compact = false,
  testIdPrefix = "patrol-schedule",
}: PatrolSchedulePickerProps) {
  const parsed = useMemo(() => parsePatrolCron(cron), [cron]);
  const [overrideForCron, setOverrideForCron] = useState<{
    cron: string;
    mode: PatrolFrequency;
  } | null>(null);
  const [customTzMode, setCustomTzMode] = useState(false);
  const frequency: PatrolFrequency =
    overrideForCron && overrideForCron.cron === cron
      ? overrideForCron.mode
      : parsed.frequency;

  const state: PatrolScheduleState = useMemo(
    () => ({
      ...parsed,
      frequency,
    }),
    [parsed, frequency],
  );

  const effectiveTz = timezone.trim() || "UTC";
  const zones = useMemo(
    () => timeZoneOptions(effectiveTz, browserTimeZone()),
    [effectiveTz],
  );

  const updateState = (patch: Partial<PatrolScheduleState>) => {
    const nextFreq = patch.frequency ?? frequency;
    const next: PatrolScheduleState = {
      ...state,
      ...patch,
      frequency: nextFreq,
    };
    const nextCron = buildPatrolCron(next);
    setOverrideForCron({ cron: nextCron, mode: nextFreq });
    onChangeCron(nextCron);
  };

  const handleFrequencyChange = (freq: PatrolFrequency) => {
    if (freq === "custom_cron") {
      const nextCron = cron.trim() || "0 */2 * * *";
      setOverrideForCron({ cron: nextCron, mode: "custom_cron" });
      onChangeCron(nextCron);
      return;
    }
    const next: PatrolScheduleState = {
      ...state,
      frequency: freq,
      days:
        freq === "weekly" && state.days.length === 0
          ? [1, 3, 5]
          : state.days,
    };
    const nextCron = buildPatrolCron(next);
    setOverrideForCron({ cron: nextCron, mode: freq });
    onChangeCron(nextCron);
  };

  const toggleDay = (dow: number) => {
    const has = state.days.includes(dow);
    const nextDays = has
      ? state.days.length > 1
        ? state.days.filter((d) => d !== dow)
        : state.days
      : sortPatrolDays([...state.days, dow]);
    updateState({ frequency: "weekly", days: nextDays });
  };

  const humanSummary = describeCronExpression(cron);
  const labelClass = compact
    ? "grid gap-1 text-[0.68rem] font-medium text-[var(--color-text-muted)]"
    : "grid gap-1 text-[0.75rem] font-medium text-[var(--color-text-secondary)]";
  const inputClass = compact
    ? "w-full rounded-[0.45rem] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-2 py-1 text-[0.75rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
    : "h-9 w-full rounded-[0.45rem] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-2.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]";

  return (
    <div
      data-testid={testIdPrefix}
      className="grid gap-2.5 rounded-[0.55rem] border border-[var(--color-border)] bg-[var(--color-surface-inset)] p-2.5"
    >
      <div
        className={`grid gap-2 ${
          frequency === "custom_cron" ? "sm:grid-cols-1" : "sm:grid-cols-2"
        }`}
      >
        <label className={labelClass}>
          <span className="flex items-center justify-between gap-2">
            <span>Repeat</span>
            {frequency === "custom_cron" ? (
              <button
                type="button"
                onClick={() =>
                  handleFrequencyChange(
                    parsed.frequency !== "custom_cron"
                      ? parsed.frequency
                      : "weekdays",
                  )
                }
                className="pill-btn h-5 px-2 text-[0.68rem]"
              >
                Use simple schedule
              </button>
            ) : (
              <button
                type="button"
                onClick={() => handleFrequencyChange("custom_cron")}
                className="pill-btn h-5 px-2 text-[0.68rem]"
              >
                Advanced cron
              </button>
            )}
          </span>
          <select
            aria-label="Repeat"
            data-testid={`${testIdPrefix}-frequency`}
            value={frequency}
            onChange={(e) =>
              handleFrequencyChange(e.target.value as PatrolFrequency)
            }
            className={inputClass}
          >
            <option value="hours">Every N hours</option>
            <option value="minutes">Every N minutes</option>
            <option value="daily">Every day</option>
            <option value="weekdays">Weekdays (Mon–Fri)</option>
            <option value="weekly">Specific days of the week</option>
            <option value="custom_cron">Advanced (cron expression)</option>
          </select>
        </label>

        {frequency === "hours" ? (
          <label className={labelClass}>
            <span>Interval</span>
            <select
              aria-label="Hour interval"
              data-testid={`${testIdPrefix}-hours`}
              value={state.intervalHours}
              onChange={(e) =>
                updateState({ intervalHours: Number(e.target.value) })
              }
              className={inputClass}
            >
              {HOUR_INTERVAL_OPTIONS.map((hrs) => (
                <option key={hrs} value={hrs}>
                  {hrs === 1 ? "Every hour" : `Every ${hrs} hours`}
                </option>
              ))}
            </select>
          </label>
        ) : null}

        {frequency === "minutes" ? (
          <label className={labelClass}>
            <span>Interval</span>
            <select
              aria-label="Minute interval"
              data-testid={`${testIdPrefix}-minutes`}
              value={state.intervalMinutes}
              onChange={(e) =>
                updateState({ intervalMinutes: Number(e.target.value) })
              }
              className={inputClass}
            >
              {MINUTE_INTERVAL_OPTIONS.map((mins) => (
                <option key={mins} value={mins}>
                  Every {mins} minutes
                </option>
              ))}
            </select>
          </label>
        ) : null}

        {frequency === "daily" ||
        frequency === "weekdays" ||
        frequency === "weekly" ? (
          <label className={labelClass}>
            <span>At time</span>
            <input
              type="time"
              aria-label="At time"
              data-testid={`${testIdPrefix}-time`}
              value={state.time}
              onChange={(e) =>
                updateState({ time: e.target.value || "09:00" })
              }
              className={inputClass}
            />
          </label>
        ) : null}
      </div>

      {frequency === "weekly" ? (
        <div className="grid gap-1">
          <span
            className={
              compact
                ? "text-[0.68rem] font-medium text-[var(--color-text-muted)]"
                : "text-[0.75rem] font-medium text-[var(--color-text-secondary)]"
            }
          >
            On days
          </span>
          <div
            role="group"
            aria-label="Days of the week"
            className="flex flex-wrap gap-1.5"
          >
            {PATROL_WEEKDAYS.map((day) => {
              const active = state.days.includes(day.dow);
              return (
                <button
                  key={day.dow}
                  type="button"
                  aria-label={day.long}
                  aria-pressed={active}
                  onClick={() => toggleDay(day.dow)}
                  className="pill-btn hit-area-touch h-7 px-2.5 text-[0.72rem]"
                >
                  {day.short}
                </button>
              );
            })}
          </div>
        </div>
      ) : null}

      {frequency === "custom_cron" ? (
        <label className={labelClass}>
          <span>Cron expression (5 fields: min hour day month weekday)</span>
          <input
            type="text"
            aria-label="Cron expression"
            data-testid={`${testIdPrefix}-raw-cron`}
            value={cron}
            onChange={(e) => {
              const val = e.target.value;
              setOverrideForCron({ cron: val, mode: "custom_cron" });
              onChangeCron(val);
            }}
            placeholder="0 9 * * 1-5"
            className={`${inputClass} font-mono`}
          />
        </label>
      ) : null}

      {onChangeTimezone ? (
        <label className={labelClass}>
          <span className="flex items-center justify-between gap-2">
            <span>Time zone</span>
            <button
              type="button"
              onClick={() => {
                if (customTzMode && !timezone.trim()) {
                  onChangeTimezone("UTC");
                }
                setCustomTzMode((v) => !v);
              }}
              className="pill-btn h-5 px-2 text-[0.68rem]"
            >
              {customTzMode ? "Pick from list" : "Custom zone"}
            </button>
          </span>
          {customTzMode ? (
            <input
              type="text"
              aria-label="Time zone"
              data-testid={`${testIdPrefix}-timezone-custom`}
              value={timezone}
              onChange={(e) => onChangeTimezone(e.target.value)}
              onBlur={() => {
                if (!timezone.trim()) {
                  onChangeTimezone("UTC");
                }
              }}
              placeholder="America/New_York or UTC"
              className={`${inputClass} font-mono`}
            />
          ) : (
            <select
              aria-label="Time zone"
              data-testid={`${testIdPrefix}-timezone`}
              value={effectiveTz}
              onChange={(e) => onChangeTimezone(e.target.value)}
              className={inputClass}
            >
              {zones.map((z) => (
                <option key={z} value={z}>
                  {formatTimeZoneLabel(z)}
                </option>
              ))}
            </select>
          )}
        </label>
      ) : null}

      <div className="flex flex-wrap items-center justify-between gap-2 text-[0.68rem] text-[var(--color-text-muted)]">
        <span>
          {humanSummary ? (
            <>
              <strong className="font-medium text-[var(--color-text-secondary)]">
                Schedule:
              </strong>{" "}
              {humanSummary} ({formatTimeZoneLabel(effectiveTz)})
            </>
          ) : (
            "Enter a valid 5-field cron expression."
          )}
        </span>
      </div>
    </div>
  );
}
