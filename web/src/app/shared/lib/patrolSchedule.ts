// Shared schedule builder + parser for recurring patrols (Chat Composer Patrol
// popover and Settings → Personas patrol editor). Mirrors the Operations
// Center's simple schedule builder (TaskCreateModal) while also covering
// sub-daily interval patrols (every N minutes / every N hours) and keeping raw
// 5-field cron as an optional backup mode.

import { describeCronExpression, describeCronExpressionShort } from "./cron";

export type PatrolFrequency =
  | "minutes"
  | "hours"
  | "daily"
  | "weekdays"
  | "weekly"
  | "custom_cron";

export type PatrolScheduleState = {
  frequency: PatrolFrequency;
  intervalMinutes: number;
  intervalHours: number;
  time: string; // "HH:MM" (24-hour)
  days: number[]; // 0=Sun .. 6=Sat
  rawCron: string;
};

export const MINUTE_INTERVAL_OPTIONS = [5, 10, 15, 20, 30] as const;
export const HOUR_INTERVAL_OPTIONS = [1, 2, 3, 4, 6, 8, 12] as const;

export const PATROL_WEEKDAYS: ReadonlyArray<{
  dow: number;
  short: string;
  long: string;
}> = [
  { dow: 1, short: "Mon", long: "Monday" },
  { dow: 2, short: "Tue", long: "Tuesday" },
  { dow: 3, short: "Wed", long: "Wednesday" },
  { dow: 4, short: "Thu", long: "Thursday" },
  { dow: 5, short: "Fri", long: "Friday" },
  { dow: 6, short: "Sat", long: "Saturday" },
  { dow: 0, short: "Sun", long: "Sunday" },
];

const WEEKDAY_ORDER = [1, 2, 3, 4, 5, 6, 0];

export function sortPatrolDays(days: number[]): number[] {
  const unique = Array.from(new Set(days.filter((d) => d >= 0 && d <= 6)));
  return unique.sort((a, b) => WEEKDAY_ORDER.indexOf(a) - WEEKDAY_ORDER.indexOf(b));
}

function parseTimeParts(time: string): { hour: number; minute: number } {
  const match = /^(\d{1,2}):(\d{2})$/.exec(time.trim());
  const hour = match ? Math.min(23, Math.max(0, Number(match[1]))) : 9;
  const minute = match ? Math.min(59, Math.max(0, Number(match[2]))) : 0;
  return { hour, minute };
}

function pad2(n: number): string {
  return String(n).padStart(2, "0");
}

export function buildPatrolCron(state: PatrolScheduleState): string {
  switch (state.frequency) {
    case "minutes": {
      const m = Math.min(59, Math.max(1, Math.round(state.intervalMinutes || 30)));
      return `*/${m} * * * *`;
    }
    case "hours": {
      const h = Math.min(23, Math.max(1, Math.round(state.intervalHours || 2)));
      return h === 1 ? "0 * * * *" : `0 */${h} * * *`;
    }
    case "daily": {
      const { hour, minute } = parseTimeParts(state.time);
      return `${minute} ${hour} * * *`;
    }
    case "weekdays": {
      const { hour, minute } = parseTimeParts(state.time);
      return `${minute} ${hour} * * 1-5`;
    }
    case "weekly": {
      const { hour, minute } = parseTimeParts(state.time);
      const sorted = sortPatrolDays(state.days);
      const picked = sorted.length > 0 ? sorted : [1];
      return `${minute} ${hour} * * ${picked.join(",")}`;
    }
    case "custom_cron":
      return state.rawCron.trim();
  }
}

export function parsePatrolCron(cron: string): PatrolScheduleState {
  const trimmed = (cron || "").trim();
  const base: PatrolScheduleState = {
    frequency: "hours",
    intervalMinutes: 30,
    intervalHours: 2,
    time: "09:00",
    days: [1, 2, 3, 4, 5],
    rawCron: trimmed || "0 */2 * * *",
  };

  if (!trimmed) return base;

  const fields = trimmed.split(/\s+/);
  if (fields.length !== 5) {
    return { ...base, frequency: "custom_cron", rawCron: trimmed };
  }

  const [minField, hourField, domField, monthField, dowField] = fields;

  if (domField === "*" && monthField === "*" && dowField === "*") {
    // Every N minutes: */N * * * *
    const minStep = /^\*\/(\d+)$/.exec(minField);
    if (minStep && hourField === "*") {
      const mins = Number(minStep[1]);
      if (
        (MINUTE_INTERVAL_OPTIONS as readonly number[]).includes(mins)
      ) {
        return {
          ...base,
          frequency: "minutes",
          intervalMinutes: mins,
          rawCron: trimmed,
        };
      }
    }

    // Every hour: 0 * * * * or 0 */1 * * *
    if (minField === "0" && (hourField === "*" || hourField === "*/1")) {
      return {
        ...base,
        frequency: "hours",
        intervalHours: 1,
        rawCron: trimmed,
      };
    }

    // Every N hours: 0 */N * * *
    const hourStep = /^\*\/(\d+)$/.exec(hourField);
    if (minField === "0" && hourStep) {
      const hrs = Number(hourStep[1]);
      if ((HOUR_INTERVAL_OPTIONS as readonly number[]).includes(hrs)) {
        return {
          ...base,
          frequency: "hours",
          intervalHours: hrs,
          rawCron: trimmed,
        };
      }
    }
  }

  // Fixed time of day patterns: M H * * <dow>
  if (
    domField === "*" &&
    monthField === "*" &&
    /^\d{1,2}$/.test(minField) &&
    /^\d{1,2}$/.test(hourField)
  ) {
    const minute = Number(minField);
    const hour = Number(hourField);
    if (minute >= 0 && minute <= 59 && hour >= 0 && hour <= 23) {
      const time = `${pad2(hour)}:${pad2(minute)}`;
      if (dowField === "*") {
        return {
          ...base,
          frequency: "daily",
          time,
          days: [1, 2, 3, 4, 5, 6, 0],
          rawCron: trimmed,
        };
      }
      if (dowField === "1-5" || dowField.toUpperCase() === "MON-FRI") {
        return {
          ...base,
          frequency: "weekdays",
          time,
          days: [1, 2, 3, 4, 5],
          rawCron: trimmed,
        };
      }
      if (/^[0-7](,[0-7])*$/.test(dowField)) {
        const parsedDays = sortPatrolDays(
          dowField.split(",").map((d) => Number(d) % 7),
        );
        if (parsedDays.length > 0) {
          return {
            ...base,
            frequency: "weekly",
            time,
            days: parsedDays,
            rawCron: trimmed,
          };
        }
      }
    }
  }

  return {
    ...base,
    frequency: "custom_cron",
    rawCron: trimmed,
  };
}

/**
 * Compact human-readable cadence for badges and banners (e.g. "Every hour",
 * "Every 2h", "Weekdays 09:00", "Daily 09:00", or fallback to
 * describeCronExpressionShort / describeCronExpression).
 */
export function formatCompactCronCadence(cron: string): string {
  const c = (cron || "").trim();
  if (!c) return "Recurring";
  if (c === "0 * * * *" || c === "0 */1 * * *") return "Every hour";
  const everyNHours = /^0 \*\/(\d+) \* \* \*$/.exec(c);
  if (everyNHours) return `Every ${everyNHours[1]}h`;
  const everyNMinutes = /^\*\/(\d+) \* \* \* \*$/.exec(c);
  if (everyNMinutes) return `Every ${everyNMinutes[1]}m`;
  const daily = /^(\d{1,2}) (\d{1,2}) \* \* \*$/.exec(c);
  if (daily) {
    const m = pad2(Number(daily[1]));
    const h = pad2(Number(daily[2]));
    return `Daily ${h}:${m}`;
  }
  const weekdays = /^(\d{1,2}) (\d{1,2}) \* \* (?:1-5|MON-FRI)$/i.exec(c);
  if (weekdays) {
    const m = pad2(Number(weekdays[1]));
    const h = pad2(Number(weekdays[2]));
    return `Weekdays ${h}:${m}`;
  }
  return describeCronExpressionShort(c) || describeCronExpression(c) || c;
}
