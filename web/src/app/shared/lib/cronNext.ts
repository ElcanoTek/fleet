// Next-occurrence preview for the task form's cron field. Computes when a
// 5-field numeric cron expression would next fire so the schedule echo can say
// "— next run Mon, Jul 13" before the task exists (the backend only computes
// next_run for already-created tasks).
//
// Honest scope: this is a PREVIEW, not the scheduler. It evaluates in the
// browser's local timezone, while the backend evaluates recurrence in the
// server's default timezone — so near field boundaries the previewed date can
// differ from the real first run. The echo therefore shows a date (no time of
// day) to keep the blast radius of a timezone mismatch small, and callers must
// treat null as "can't preview" and simply omit the suffix. Only the numeric
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
function dayMatches(s: CronSchedule, date: Date): boolean {
  const domOk = s.dom.has(date.getDate());
  const dowOk = s.dow.has(date.getDay());
  if (s.domRestricted && s.dowRestricted) return domOk || dowOk;
  if (s.domRestricted) return domOk;
  if (s.dowRestricted) return dowOk;
  return true;
}

// nextCronOccurrence returns the first local-time occurrence strictly after
// `from`, or null when the expression can't be previewed (unsupported syntax)
// or nothing fires within the next 366 days.
export function nextCronOccurrence(expr: string, from: Date = new Date()): Date | null {
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
    if (s.months.has(day.getMonth() + 1) && dayMatches(s, day)) {
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
// "Mon, Jul 13" — date only (see the timezone note above).
export function formatNextRun(date: Date): string {
  return date.toLocaleDateString("en-US", { weekday: "short", month: "short", day: "numeric" });
}
