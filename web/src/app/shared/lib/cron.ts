import { t as tr } from "@/app/lib/i18n";
import { getLanguage } from "@/app/lib/i18n";
// Cron expression → human-readable description. Ported verbatim (logic-for-
// logic) from moc's assets/js/utils.js. Translates a 5-field cron expression
// into plain English so users can verify a schedule without leaving the form.
// Returns "" for inputs it cannot describe so callers can simply hide the line.

const CRON_MONTHS = [
  "",
  tr("January"),
  tr("February"),
  tr("March"),
  tr("April"),
  tr("May"),
  tr("June"),
  tr("July"),
  tr("August"),
  tr("September"),
  tr("October"),
  tr("November"),
  tr("December"),
];
const CRON_DAYS = [tr("Sunday"), tr("Monday"), tr("Tuesday"), tr("Wednesday"), tr("Thursday"), tr("Friday"), tr("Saturday")];
const CRON_MONTH_NAMES: Record<string, number> = {
  JAN: 1, FEB: 2, MAR: 3, APR: 4, MAY: 5, JUN: 6, JUL: 7, AUG: 8, SEP: 9, OCT: 10, NOV: 11, DEC: 12,
};
const CRON_DAY_NAMES: Record<string, number> = {
  SUN: 0, MON: 1, TUE: 2, WED: 3, THU: 4, FRI: 5, SAT: 6,
};

function pad(n: number): string {
  return String(n).padStart(2, "0");
}

function ordinal(n: number): string {
  if (getLanguage() !== "en") return String(n);
  if (n >= 11 && n <= 13) return `${n}th`;
  switch (n % 10) {
    case 1: return `${n}st`;
    case 2: return `${n}nd`;
    case 3: return `${n}rd`;
    default: return `${n}th`;
  }
}

function joinList(arr: string[]): string {
  if (arr.length === 0) return "";
  if (arr.length === 1) return arr[0];
  if (arr.length === 2) return tr("{0} and {1}", [arr[0], arr[1]]);
  return arr.slice(0, -1).join(", ") + tr(", and ") + arr[arr.length - 1];
}

function describeTime(minute: string, hour: string): string {
  const minIsNum = /^\d+$/.test(minute);
  const hourIsNum = /^\d+$/.test(hour);
  const minStep = minute.match(/^\*\/(\d+)$/);
  const hourStep = hour.match(/^\*\/(\d+)$/);
  const hourRange = hour.match(/^(\d+)-(\d+)$/);

  if (minIsNum && hourIsNum) {
    const m = parseInt(minute, 10);
    const h = parseInt(hour, 10);
    if (m < 0 || m > 59 || h < 0 || h > 23) throw new Error("time out of range");
    return tr("At {0}:{1}", [pad(h), pad(m)]);
  }
  if (minute === "*" && hour === "*") return tr("Every minute");
  if (minStep && hour === "*") return tr("Every {0} minutes", [minStep[1]]);
  if (minIsNum && hour === "*") return tr("At minute {0} of every hour", [parseInt(minute, 10)]);
  if (minute === "*" && hourIsNum) {
    const h = parseInt(hour, 10);
    return tr("Every minute between {0}:00 and {1}:59", [pad(h), pad(h)]);
  }
  if (minStep && hourIsNum) {
    const h = parseInt(hour, 10);
    return tr("Every {0} minutes between {1}:00 and {2}:59", [minStep[1], pad(h), pad(h)]);
  }
  if (minIsNum && hourStep) {
    return tr("At minute {0} past every {1} hour", [parseInt(minute, 10), ordinal(parseInt(hourStep[1], 10))]);
  }
  if (minIsNum && hourRange) {
    const h1 = parseInt(hourRange[1], 10);
    const h2 = parseInt(hourRange[2], 10);
    return tr("At minute {0} past every hour from {1}:00 through {2}:59", [parseInt(minute, 10), pad(h1), pad(h2)]);
  }
  if (minIsNum && /^\d+(,\d+)+$/.test(hour)) {
    const m = parseInt(minute, 10);
    const stamps = hour.split(",").map((h) => `${pad(parseInt(h, 10))}:${pad(m)}`);
    return tr("At {0}", [joinList(stamps)]);
  }
  if (/^\d+(,\d+)+$/.test(minute) && hour === "*") {
    const mins = minute.split(",").map((m) => parseInt(m, 10));
    return tr("At minute {0} of every hour", [joinList(mins.map(String))]);
  }
  return tr("At minute {0} of hour {1}", [minute, hour]);
}

function describeDom(dom: string): string {
  if (dom === "*") return "";
  if (/^\d+$/.test(dom)) return tr("on the {0} of the month", [ordinal(parseInt(dom, 10))]);
  const range = dom.match(/^(\d+)-(\d+)$/);
  if (range) return tr("on day {0} through {1} of the month", [range[1], range[2]]);
  if (/^\d+(,\d+)+$/.test(dom)) {
    const days = dom.split(",").map((d) => ordinal(parseInt(d, 10)));
    return tr("on the {0} of the month", [joinList(days)]);
  }
  const step = dom.match(/^\*\/(\d+)$/);
  if (step) return tr("every {0} days", [step[1]]);
  return tr("on day-of-month {0}", [dom]);
}

function describeDow(dow: string): string {
  if (dow === "*") return "";
  if (/^\d+$/.test(dow)) return tr("only on {0}", [CRON_DAYS[parseInt(dow, 10) % 7]]);
  const range = dow.match(/^(\d+)-(\d+)$/);
  if (range) {
    return tr("{0} through {1}", [CRON_DAYS[parseInt(range[1], 10) % 7], CRON_DAYS[parseInt(range[2], 10) % 7]]);
  }
  if (/^\d+(,\d+)+$/.test(dow)) {
    const days = dow.split(",").map((d) => CRON_DAYS[parseInt(d, 10) % 7]);
    return tr("only on {0}", [joinList(days)]);
  }
  const step = dow.match(/^\*\/(\d+)$/);
  if (step) return tr("every {0} days of the week", [step[1]]);
  const upper = dow.toUpperCase();
  if (CRON_DAY_NAMES[upper] !== undefined) return tr("only on {0}", [CRON_DAYS[CRON_DAY_NAMES[upper]]]);
  if (/^[A-Za-z]+-[A-Za-z]+$/.test(dow)) {
    const [a, b] = dow.split("-").map((s) => s.toUpperCase());
    if (CRON_DAY_NAMES[a] !== undefined && CRON_DAY_NAMES[b] !== undefined) {
      return tr("{0} through {1}", [CRON_DAYS[CRON_DAY_NAMES[a]], CRON_DAYS[CRON_DAY_NAMES[b]]]);
    }
  }
  return tr("on day-of-week {0}", [dow]);
}

function describeMonth(month: string): string {
  if (month === "*") return "";
  if (/^\d+$/.test(month)) {
    const m = parseInt(month, 10);
    if (m >= 1 && m <= 12) return tr("in {0}", [CRON_MONTHS[m]]);
  }
  const range = month.match(/^(\d+)-(\d+)$/);
  if (range) {
    const a = parseInt(range[1], 10);
    const b = parseInt(range[2], 10);
    if (a >= 1 && a <= 12 && b >= 1 && b <= 12) {
      return tr("{0} through {1}", [CRON_MONTHS[a], CRON_MONTHS[b]]);
    }
  }
  if (/^\d+(,\d+)+$/.test(month)) {
    const months = month.split(",").map((m) => CRON_MONTHS[parseInt(m, 10)]);
    return tr("in {0}", [joinList(months)]);
  }
  const upper = month.toUpperCase();
  if (CRON_MONTH_NAMES[upper] !== undefined) return tr("in {0}", [CRON_MONTHS[CRON_MONTH_NAMES[upper]]]);
  return tr("in month {0}", [month]);
}

export function describeCronExpression(expr: unknown): string {
  if (!expr || typeof expr !== "string") return "";
  const trimmed = expr.trim();
  if (!trimmed) return "";
  const parts = trimmed.split(/\s+/);
  if (parts.length < 5 || parts.length > 6) return "";

  const [minute, hour, dom, month, dow] = parts;
  try {
    const timeStr = describeTime(minute, hour);
    const domStr = describeDom(dom);
    const dowStr = describeDow(dow);
    const monthStr = describeMonth(month);

    let result = timeStr;
    const domAll = dom === "*";
    const dowAll = dow === "*";
    if (!domAll && !dowAll) {
      result += tr(", {0} or {1}", [domStr, dowStr]);
    } else if (!domAll) {
      result += `, ${domStr}`;
    } else if (!dowAll) {
      result += `, ${dowStr}`;
    }
    if (monthStr) result += `, ${monthStr}`;
    return result;
  } catch {
    return "";
  }
}

// Compact schedule label for dense surfaces (the Recent Tasks Schedule
// column / phone cards), time first with a middle-dot separator:
// "9:00 AM · Sat, Sun" / "8:30 AM · Mon–Fri" / "9:00 AM · Daily".
// Handles fixed-time + day-of-week shapes and
// returns "" for anything else so callers can fall back to the verbose
// describeCronExpression (which in turn falls back to the raw string).
const CRON_DAYS_SHORT = [tr("Sun"), tr("Mon"), tr("Tue"), tr("Wed"), tr("Thu"), tr("Fri"), tr("Sat")];

function dowIndex(token: string): number | null {
  if (/^\d+$/.test(token)) return parseInt(token, 10) % 7;
  const named = CRON_DAY_NAMES[token.toUpperCase()];
  return named === undefined ? null : named;
}

export function describeCronExpressionShort(expr: unknown): string {
  if (!expr || typeof expr !== "string") return "";
  const parts = expr.trim().split(/\s+/);
  if (parts.length !== 5) return "";
  const [minute, hour, dom, month, dow] = parts;
  // Compact form only covers fixed times on day-of-week patterns; months and
  // day-of-month schedules keep the verbose description.
  if (dom !== "*" || month !== "*") return "";
  if (!/^\d+$/.test(minute) || !/^\d+$/.test(hour)) return "";
  const h = parseInt(hour, 10);
  const m = parseInt(minute, 10);
  if (h > 23 || m > 59) return "";
  const time = new Date(2000, 0, 1, h, m).toLocaleTimeString(getLanguage(), {
    hour: "numeric",
    minute: "2-digit",
  });

  let days = "";
  if (dow === "*") {
    days = tr("Daily");
  } else {
    const single = dowIndex(dow);
    const range = dow.match(/^([^-]+)-([^-]+)$/);
    if (single !== null) {
      days = CRON_DAYS_SHORT[single];
    } else if (range) {
      const a = dowIndex(range[1]);
      const b = dowIndex(range[2]);
      if (a === null || b === null) return "";
      days = `${CRON_DAYS_SHORT[a]}–${CRON_DAYS_SHORT[b]}`;
    } else if (dow.includes(",")) {
      const idxs = dow.split(",").map(dowIndex);
      if (idxs.some((i) => i === null)) return "";
      days = idxs.map((i) => CRON_DAYS_SHORT[i as number]).join(", ");
    } else {
      return "";
    }
  }
  return `${time} · ${days}`;
}
