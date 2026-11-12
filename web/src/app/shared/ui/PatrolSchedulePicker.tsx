import { t as tr } from "@/app/lib/i18n";
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
import { Input } from "@/components/ui/input";
import {
  NativeSelect,
  NativeSelectOption,
} from "@/components/ui/native-select";

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
  const selectClass = compact
    ? "w-full rounded-[0.45rem] border border-[var(--color-border)] bg-[var(--color-surface-1)] py-1 pr-7 pl-2 text-[0.75rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
    : "h-9 w-full rounded-[0.45rem] border border-[var(--color-border)] bg-[var(--color-surface-1)] pr-8 pl-2.5 text-[0.8125rem] text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]";

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
            <span>{tr("Repeat")}</span>
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
                {tr("Use simple schedule")}
              </button>
            ) : (
              <button
                type="button"
                onClick={() => handleFrequencyChange("custom_cron")}
                className="pill-btn h-5 px-2 text-[0.68rem]"
              >
                {tr("Advanced cron")}
              </button>
            )}
          </span>
          <NativeSelect
            size={compact ? "sm" : "default"}
            aria-label={tr("Repeat")}
            data-testid={`${testIdPrefix}-frequency`}
            value={frequency}
            onChange={(e) =>
              handleFrequencyChange(e.target.value as PatrolFrequency)
            }
            className={selectClass}
          >
            <NativeSelectOption value="hours">{tr("Every N hours")}</NativeSelectOption>
            <NativeSelectOption value="minutes">{tr("Every N minutes")}</NativeSelectOption>
            <NativeSelectOption value="daily">{tr("Every day")}</NativeSelectOption>
            <NativeSelectOption value="weekdays">{tr("Weekdays (Mon–Fri)")}</NativeSelectOption>
            <NativeSelectOption value="weekly">{tr("Specific days of the week")}</NativeSelectOption>
            <NativeSelectOption value="custom_cron">{tr("Advanced (cron expression)")}</NativeSelectOption>
          </NativeSelect>
        </label>

        {frequency === "hours" ? (
          <label className={labelClass}>
            <span>{tr("Interval")}</span>
            <NativeSelect
              size={compact ? "sm" : "default"}
              aria-label={tr("Hour interval")}
              data-testid={`${testIdPrefix}-hours`}
              value={state.intervalHours}
              onChange={(e) =>
                updateState({ intervalHours: Number(e.target.value) })
              }
              className={selectClass}
            >
              {HOUR_INTERVAL_OPTIONS.map((hrs) => (
                <NativeSelectOption key={hrs} value={hrs}>
                  {hrs === 1 ? tr("Every hour") : tr("Every {0} hours", [hrs])}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </label>
        ) : null}

        {frequency === "minutes" ? (
          <label className={labelClass}>
            <span>{tr("Interval")}</span>
            <NativeSelect
              size={compact ? "sm" : "default"}
              aria-label={tr("Minute interval")}
              data-testid={`${testIdPrefix}-minutes`}
              value={state.intervalMinutes}
              onChange={(e) =>
                updateState({ intervalMinutes: Number(e.target.value) })
              }
              className={selectClass}
            >
              {MINUTE_INTERVAL_OPTIONS.map((mins) => (
                <NativeSelectOption key={mins} value={mins}>
                  {tr("Every {0} minutes", [mins])}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </label>
        ) : null}

        {frequency === "daily" ||
        frequency === "weekdays" ||
        frequency === "weekly" ? (
          <label className={labelClass}>
            <span>{tr("At time")}</span>
            <Input
              type="time"
              aria-label={tr("At time")}
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
            {tr("On days")}
          </span>
          <div
            role="group"
            aria-label={tr("Days of the week")}
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
          <span>{tr("Cron expression (5 fields: min hour day month weekday)")}</span>
          <Input
            type="text"
            aria-label={tr("Cron expression")}
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
            <span>{tr("Time zone")}</span>
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
              {customTzMode ? tr("Pick from list") : tr("Custom zone")}
            </button>
          </span>
          {customTzMode ? (
            <Input
              type="text"
              aria-label={tr("Time zone")}
              data-testid={`${testIdPrefix}-timezone-custom`}
              value={timezone}
              onChange={(e) => onChangeTimezone(e.target.value)}
              onBlur={() => {
                if (!timezone.trim()) {
                  onChangeTimezone("UTC");
                }
              }}
              placeholder={tr("America/New_York or UTC")}
              className={`${inputClass} font-mono`}
            />
          ) : (
            <NativeSelect
              size={compact ? "sm" : "default"}
              aria-label={tr("Time zone")}
              data-testid={`${testIdPrefix}-timezone`}
              value={effectiveTz}
              onChange={(e) => onChangeTimezone(e.target.value)}
              className={selectClass}
            >
              {zones.map((z) => (
                <NativeSelectOption key={z} value={z}>
                  {formatTimeZoneLabel(z)}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          )}
        </label>
      ) : null}

      <div className="flex flex-wrap items-center justify-between gap-2 text-[0.68rem] text-[var(--color-text-muted)]">
        <span>
          {humanSummary ? (
            <>
              <strong className="font-medium text-[var(--color-text-secondary)]">
                {tr("Schedule:")}
              </strong>{" "}
              {humanSummary} ({formatTimeZoneLabel(effectiveTz)})
            </>
          ) : (
            tr("Enter a valid 5-field cron expression.")
          )}
        </span>
      </div>
    </div>
  );
}
