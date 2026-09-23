// Next-occurrence preview for the task form's cron field. Computes when a
// 5-field numeric cron expression would next fire so the schedule echo can say
// "— next run Mon, Jul 13" before the task exists (the backend only computes
// next_run for already-created tasks).
//
// Honest scope: this is a PREVIEW, not the scheduler. Given a `timeZone` it
// evaluates in that IANA zone — the task's own, which is what the backend
// evaluates recurrence in — scanning the zone's calendar with DST-free UTC
// arithmetic so the browser's own zone never leaks in; without one it
// evaluates in the browser's local timezone. A wall-clock time the zone skips
// (a spring-forward gap) is not previewed as an occurrence. The echo shows a
// date (no time of day), and callers must treat null as "can't preview" and
// simply omit the suffix. Only the numeric
// 5-field subset the form validator accepts is supported; anything else
// (named tokens, 6-field expressions, out-of-range values) returns null.

type FieldSet = Set<number>;

// parseField expands one cron field (lists, ranges, steps, *) into the set of
// allowed values within [min, max]. Returns null when the field doesn't parse
// or names a value outside the range.
function parseField(field: string, min: number, max: number): FieldSet | null {
  const out: FieldSet = new Set();
  for (const part of field.split(",")) {
    if (part === "") return null;
    const stepMatch = part.match(/^(.+)\/(\d+)$/);
    const base = stepMatch ? stepMatch[1] : part;
    const step = stepMatch ? Number.parseInt(stepMatch[2], 10) : 1;
    if (!Number.isFinite(step) || step < 1) return null;

    let lo: number;
    let hi: number;
    if (base === "*") {
      lo = min;
      hi = max;
    } else {
      const range = base.match(/^(\d+)-(\d+)$/);
      if (range) {
        lo = Number.parseInt(range[1], 10);
        hi = Number.parseInt(range[2], 10);
      } else if (/^\d+$/.test(base)) {
        lo = Number.parseInt(base, 10);
        // "N/step" behaves like "N-max/step" in common cron dialects, but a
        // bare number without a step is just that number.
        hi = stepMatch ? max : lo;
      } else {
        return null;
      }
      if (lo < min || hi > max || lo > hi) return null;
    }
    for (let v = lo; v <= hi; v += step) out.add(v);
  }
  return out.size > 0 ? out : null;
}

export type CronSchedule = {
  minutes: FieldSet;
  hours: FieldSet;
  dom: FieldSet;
  months: FieldSet;
  dow: FieldSet;
  domRestricted: boolean;
  dowRestricted: boolean;
};

export function parseCronExpression(expr: string): CronSchedule | null {
  const parts = expr.trim().split(/\s+/);
  if (parts.length !== 5) return null;
  const [minute, hour, dom, month, dow] = parts;
  const minutes = parseField(minute, 0, 59);
  const hours = parseField(hour, 0, 23);
  const domSet = parseField(dom, 1, 31);
  const months = parseField(month, 1, 12);
  // Accept 0–7 with 7 meaning Sunday (normalized to 0), per common cron.
  const dowRaw = parseField(dow, 0, 7);
  if (!minutes || !hours || !domSet || !months || !dowRaw) return null;
  const dowSet: FieldSet = new Set([...dowRaw].map((d) => d % 7));
  return {
    minutes,
    hours,
    dom: domSet,
    months,
    dow: dowSet,
    domRestricted: dom !== "*",
    dowRestricted: dow !== "*",
  };
}

// dayMatches applies standard cron day semantics: when BOTH day-of-month and
// day-of-week are restricted the day matches if EITHER does; otherwise the
// restricted one (or neither) decides.
function dayMatches(s: CronSchedule, dayOfMonth: number, dayOfWeek: number): boolean {
  const domOk = s.dom.has(dayOfMonth);
  const dowOk = s.dow.has(dayOfWeek);
  if (s.domRestricted && s.dowRestricted) return domOk || dowOk;
  if (s.domRestricted) return domOk;
  if (s.dowRestricted) return dowOk;
  return true;
}

// wallClockParts reads the calendar fields `at` shows in an IANA zone.
function wallClockParts(at: Date, timeZone: string): number[] | null {
  try {
    const parts = new Intl.DateTimeFormat("en-US", {
      timeZone,
      hourCycle: "h23",
      year: "numeric",
      month: "numeric",
      day: "numeric",
      hour: "numeric",
      minute: "numeric",
      second: "numeric",
    }).formatToParts(at);
    const get = (type: string) => Number(parts.find((p) => p.type === type)?.value);
    const fields = [get("year"), get("month"), get("day"), get("hour"), get("minute"), get("second")];
    return fields.every(Number.isFinite) ? fields : null;
  } catch {
    return null;
  }
}

// zoneOffsetMs is how far the zone's wall clock runs ahead of UTC at `at`.
function zoneOffsetMs(at: Date, timeZone: string): number | null {
  const f = wallClockParts(at, timeZone);
  if (!f) return null;
  return Date.UTC(f[0], f[1] - 1, f[2], f[3], f[4], f[5]) - Math.floor(at.getTime() / 1000) * 1000;
}

// nextCronOccurrence returns the first occurrence strictly after `from` — in
// `timeZone` when given, else in local time — or null when the expression
// can't be previewed (unsupported syntax, unknown zone) or nothing fires
// within the next 366 days.
export function nextCronOccurrence(
  expr: string,
  from: Date = new Date(),
  timeZone?: string,
): Date | null {
  if (!timeZone) return nextLocalOccurrence(expr, from);
  return nextZonedOccurrence(expr, from, timeZone);
}

const MINUTE_MS = 60_000;
const DAY_MS = 86_400_000;

// nextZonedOccurrence scans the zone's calendar as "wall clock encoded as a
// UTC epoch" — UTC has no DST, so only the target zone's rules ever apply —
// and maps each candidate wall-clock time to the real instant(s) that show it.
// Candidates are compared as instants, not wall-clock readings: in a fall-back
// hour one wall time happens twice, and the second can still be ahead of
// `from` (robfig/cron's Next fires it too).
function nextZonedOccurrence(expr: string, from: Date, timeZone: string): Date | null {
  const s = parseCronExpression(expr);
  if (!s) return null;
  const f = wallClockParts(from, timeZone);
  if (!f) return null;

  const minutes = [...s.minutes].sort((a, b) => a - b);
  const hours = [...s.hours].sort((a, b) => a - b);

  // Strictly after `from`, at whole-minute resolution.
  const after = Math.floor(from.getTime() / MINUTE_MS) * MINUTE_MS + MINUTE_MS;
  // Start a day early: a transition at midnight can put the next instant on
  // an earlier wall-clock date than `from` reads.
  let day = Date.UTC(f[0], f[1] - 1, f[2]) - DAY_MS;
  for (let i = 0; i <= 367; i++, day += DAY_MS) {
    const d = new Date(day);
    if (!s.months.has(d.getUTCMonth() + 1) || !dayMatches(s, d.getUTCDate(), d.getUTCDay())) continue;
    // Wall order and instant order disagree inside a repeated hour, so take
    // the earliest qualifying instant of the whole day.
    let best: number | null = null;
    for (const h of hours) {
      for (const m of minutes) {
        const candidates = wallToInstants(day + h * 3_600_000 + m * MINUTE_MS, timeZone);
        if (candidates === null) return null;
        for (const instant of candidates) {
          if (instant < after) continue;
          // A wall time the zone skips (spring-forward) maps to a different
          // reading; it is not an occurrence.
          const got = wallClockParts(new Date(instant), timeZone);
          if (!got || got[3] !== h || got[4] !== m) continue;
          if (best === null || instant < best) best = instant;
        }
      }
    }
    if (best !== null) return new Date(best);
  }
  return null;
}

// wallToInstants maps a zone wall-clock time (encoded as a UTC epoch) to the
// instants that could show it: one per offset in effect 12h either side (no
// zone changes offset twice within a day). Normally both agree; in a
// fall-back hour they are the two real instants, and in a spring-forward gap
// neither reads back as the requested time (the caller filters that).
function wallToInstants(wallAsUTC: number, timeZone: string): number[] | null {
  const before = zoneOffsetMs(new Date(wallAsUTC - DAY_MS / 2), timeZone);
  const later = zoneOffsetMs(new Date(wallAsUTC + DAY_MS / 2), timeZone);
  if (before === null || later === null) return null;
  return before === later ? [wallAsUTC - before] : [wallAsUTC - before, wallAsUTC - later];
}

function nextLocalOccurrence(expr: string, from: Date): Date | null {
  const s = parseCronExpression(expr);
  if (!s) return null;

  const minutes = [...s.minutes].sort((a, b) => a - b);
  const hours = [...s.hours].sort((a, b) => a - b);

  // Start at the next whole minute, then scan day-by-day (≤ 366 iterations),
  // picking the earliest allowed hour:minute on the first matching day.
  const start = new Date(from.getTime());
  start.setSeconds(0, 0);
  start.setMinutes(start.getMinutes() + 1);

  const day = new Date(start.getTime());
  for (let i = 0; i <= 366; i++) {
    if (s.months.has(day.getMonth() + 1) && dayMatches(s, day.getDate(), day.getDay())) {
      const isFirstDay = i === 0;
      for (const h of hours) {
        for (const m of minutes) {
          if (isFirstDay) {
            const hh = start.getHours();
            const mm = start.getMinutes();
            if (h < hh || (h === hh && m < mm)) continue;
          }
          const candidate = new Date(day.getTime());
          candidate.setHours(h, m, 0, 0);
          // Guard DST gaps: setHours can land on a different wall-clock hour;
          // skip candidates the clock can't actually show.
          if (candidate.getHours() !== h || candidate.getMinutes() !== m) continue;
          return candidate;
        }
      }
    }
    day.setDate(day.getDate() + 1);
    day.setHours(0, 0, 0, 0);
  }
  return null;
}

// formatNextRun renders an occurrence the way the schedule echo shows it:
// "Mon, Jul 13" — date only (see the timezone note above), read in `timeZone`
// when given so the date matches the zone the schedule fires in.
export function formatNextRun(date: Date, timeZone?: string): string {
  const opts: Intl.DateTimeFormatOptions = { weekday: "short", month: "short", day: "numeric" };
  if (timeZone) opts.timeZone = timeZone;
  try {
    return date.toLocaleDateString("en-US", opts);
  } catch {
    return date.toLocaleDateString("en-US", { weekday: "short", month: "short", day: "numeric" });
  }
}
