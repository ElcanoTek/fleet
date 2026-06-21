// Cron expression → human-readable description. Ported verbatim (logic-for-
// logic) from moc's assets/js/utils.js. Translates a 5-field cron expression
// into plain English so users can verify a schedule without leaving the form.
// Returns "" for inputs it cannot describe so callers can simply hide the line.

const CRON_MONTHS = [
  "",
  "January",
  "February",
  "March",
  "April",
  "May",
  "June",
  "July",
  "August",
  "September",
  "October",
  "November",
  "December",
];
const CRON_DAYS = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];
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
  if (arr.length === 2) return `${arr[0]} and ${arr[1]}`;
  return arr.slice(0, -1).join(", ") + ", and " + arr[arr.length - 1];
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
    return `At ${pad(h)}:${pad(m)}`;
  }
  if (minute === "*" && hour === "*") return "Every minute";
  if (minStep && hour === "*") return `Every ${minStep[1]} minutes`;
  if (minIsNum && hour === "*") return `At minute ${parseInt(minute, 10)} of every hour`;
  if (minute === "*" && hourIsNum) {
    const h = parseInt(hour, 10);
    return `Every minute between ${pad(h)}:00 and ${pad(h)}:59`;
  }
  if (minStep && hourIsNum) {
    const h = parseInt(hour, 10);
    return `Every ${minStep[1]} minutes between ${pad(h)}:00 and ${pad(h)}:59`;
  }
  if (minIsNum && hourStep) {
    return `At minute ${parseInt(minute, 10)} past every ${ordinal(parseInt(hourStep[1], 10))} hour`;
  }
  if (minIsNum && hourRange) {
    const h1 = parseInt(hourRange[1], 10);
    const h2 = parseInt(hourRange[2], 10);
    return `At minute ${parseInt(minute, 10)} past every hour from ${pad(h1)}:00 through ${pad(h2)}:59`;
  }
  if (minIsNum && /^\d+(,\d+)+$/.test(hour)) {
    const m = parseInt(minute, 10);
    const stamps = hour.split(",").map((h) => `${pad(parseInt(h, 10))}:${pad(m)}`);
    return `At ${joinList(stamps)}`;
  }
  if (/^\d+(,\d+)+$/.test(minute) && hour === "*") {
    const mins = minute.split(",").map((m) => parseInt(m, 10));
    return `At minute ${joinList(mins.map(String))} of every hour`;
  }
  return `At minute ${minute} of hour ${hour}`;
}

function describeDom(dom: string): string {
  if (dom === "*") return "";
  if (/^\d+$/.test(dom)) return `on the ${ordinal(parseInt(dom, 10))} of the month`;
  const range = dom.match(/^(\d+)-(\d+)$/);
  if (range) return `on day ${range[1]} through ${range[2]} of the month`;
  if (/^\d+(,\d+)+$/.test(dom)) {
    const days = dom.split(",").map((d) => ordinal(parseInt(d, 10)));
    return `on the ${joinList(days)} of the month`;
  }
  const step = dom.match(/^\*\/(\d+)$/);
  if (step) return `every ${step[1]} days`;
  return `on day-of-month ${dom}`;
}

function describeDow(dow: string): string {
  if (dow === "*") return "";
  if (/^\d+$/.test(dow)) return `only on ${CRON_DAYS[parseInt(dow, 10) % 7]}`;
  const range = dow.match(/^(\d+)-(\d+)$/);
  if (range) {
    return `${CRON_DAYS[parseInt(range[1], 10) % 7]} through ${CRON_DAYS[parseInt(range[2], 10) % 7]}`;
  }
  if (/^\d+(,\d+)+$/.test(dow)) {
    const days = dow.split(",").map((d) => CRON_DAYS[parseInt(d, 10) % 7]);
    return `only on ${joinList(days)}`;
  }
  const step = dow.match(/^\*\/(\d+)$/);
  if (step) return `every ${step[1]} days of the week`;
  const upper = dow.toUpperCase();
  if (CRON_DAY_NAMES[upper] !== undefined) return `only on ${CRON_DAYS[CRON_DAY_NAMES[upper]]}`;
  if (/^[A-Za-z]+-[A-Za-z]+$/.test(dow)) {
    const [a, b] = dow.split("-").map((s) => s.toUpperCase());
    if (CRON_DAY_NAMES[a] !== undefined && CRON_DAY_NAMES[b] !== undefined) {
      return `${CRON_DAYS[CRON_DAY_NAMES[a]]} through ${CRON_DAYS[CRON_DAY_NAMES[b]]}`;
    }
  }
  return `on day-of-week ${dow}`;
}

function describeMonth(month: string): string {
  if (month === "*") return "";
  if (/^\d+$/.test(month)) {
    const m = parseInt(month, 10);
    if (m >= 1 && m <= 12) return `in ${CRON_MONTHS[m]}`;
  }
  const range = month.match(/^(\d+)-(\d+)$/);
  if (range) {
    const a = parseInt(range[1], 10);
    const b = parseInt(range[2], 10);
    if (a >= 1 && a <= 12 && b >= 1 && b <= 12) {
      return `${CRON_MONTHS[a]} through ${CRON_MONTHS[b]}`;
    }
  }
  if (/^\d+(,\d+)+$/.test(month)) {
    const months = month.split(",").map((m) => CRON_MONTHS[parseInt(m, 10)]);
    return `in ${joinList(months)}`;
  }
  const upper = month.toUpperCase();
  if (CRON_MONTH_NAMES[upper] !== undefined) return `in ${CRON_MONTHS[CRON_MONTH_NAMES[upper]]}`;
  return `in month ${month}`;
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
      result += `, ${domStr} or ${dowStr}`;
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
