import { describe, expect, it } from "vitest";
import { nextCronOccurrence, parseCronExpression, formatNextRun } from "./cronNext";

// Fixed reference: Friday 2026-07-10 15:30 local time.
const FRIDAY = new Date(2026, 6, 10, 15, 30, 0, 0);

describe("nextCronOccurrence", () => {
  it("weekdays 9am from a Friday afternoon lands on Monday 09:00", () => {
    const next = nextCronOccurrence("0 9 * * 1-5", FRIDAY);
    expect(next).not.toBeNull();
    expect(next!.getDay()).toBe(1); // Monday
    expect(next!.getDate()).toBe(13);
    expect(next!.getHours()).toBe(9);
    expect(next!.getMinutes()).toBe(0);
  });

  it("same-day occurrence when the time is still ahead", () => {
    const next = nextCronOccurrence("0 17 * * *", FRIDAY);
    expect(next!.getDate()).toBe(10);
    expect(next!.getHours()).toBe(17);
  });

  it("is strictly after `from` (an occurrence at the current minute rolls over)", () => {
    const at = new Date(2026, 6, 10, 9, 0, 0, 0);
    const next = nextCronOccurrence("0 9 * * *", at);
    expect(next!.getDate()).toBe(11);
  });

  it("day-of-month schedules", () => {
    const next = nextCronOccurrence("30 6 1 * *", FRIDAY);
    expect(next!.getMonth()).toBe(7); // August
    expect(next!.getDate()).toBe(1);
    expect(next!.getHours()).toBe(6);
    expect(next!.getMinutes()).toBe(30);
  });

  it("dom OR dow when both are restricted (standard cron semantics)", () => {
    // 11th of the month OR Monday — from Friday Jul 10, the 11th (Saturday)
    // comes before Monday the 13th.
    const next = nextCronOccurrence("0 9 11 * 1", FRIDAY);
    expect(next!.getDate()).toBe(11);
  });

  it("step expressions", () => {
    const next = nextCronOccurrence("*/15 * * * *", FRIDAY);
    expect(next!.getMinutes()).toBe(45);
    expect(next!.getHours()).toBe(15);
  });

  it("weekday 7 means Sunday", () => {
    const next = nextCronOccurrence("0 9 * * 7", FRIDAY);
    expect(next!.getDay()).toBe(0);
  });

  it("returns null for unsupported or invalid syntax", () => {
    expect(nextCronOccurrence("0 9 * *", FRIDAY)).toBeNull(); // 4 fields
    expect(nextCronOccurrence("0 9 * * 1 2026", FRIDAY)).toBeNull(); // 6 fields
    expect(nextCronOccurrence("0 9 * * MON", FRIDAY)).toBeNull(); // named token
    expect(nextCronOccurrence("99 9 * * *", FRIDAY)).toBeNull(); // out of range
    expect(nextCronOccurrence("0 9 * 13 *", FRIDAY)).toBeNull(); // bad month
  });

  it("returns null when nothing fires within a year (Feb 30)", () => {
    expect(nextCronOccurrence("0 9 30 2 *", FRIDAY)).toBeNull();
  });
});

describe("parseCronExpression", () => {
  it("expands lists and ranges", () => {
    const s = parseCronExpression("0 13 * * 1,4")!;
    expect([...s.dow].sort()).toEqual([1, 4]);
    expect(s.dowRestricted).toBe(true);
    expect(s.domRestricted).toBe(false);
  });
});

describe("formatNextRun", () => {
  it("renders the echo's date shape", () => {
    expect(formatNextRun(new Date(2026, 6, 13))).toBe("Mon, Jul 13");
  });
});

describe("nextCronOccurrence in a named time zone", () => {
  // 2026-09-23 12:00 UTC = 08:00 EDT, a Wednesday.
  const NOON_UTC = new Date(Date.UTC(2026, 8, 23, 12, 0, 0));

  it("fires at the zone's wall clock, not UTC's", () => {
    const next = nextCronOccurrence("0 8 * * 1-5", NOON_UTC, "America/New_York");
    // Next weekday 08:00 EDT is Thu Sep 24, 12:00 UTC.
    expect(next!.toISOString()).toBe("2026-09-24T12:00:00.000Z");
    expect(formatNextRun(next!, "America/New_York")).toBe("Thu, Sep 24");
  });

  it("UTC evaluates in UTC", () => {
    const next = nextCronOccurrence("0 8 * * 1-5", NOON_UTC, "UTC");
    expect(next!.toISOString()).toBe("2026-09-24T08:00:00.000Z");
  });

  it("uses the target date's offset across a DST change", () => {
    // New York leaves DST on 2026-11-01: 08:00 EST is 13:00 UTC.
    const from = new Date(Date.UTC(2026, 9, 31, 20, 0, 0));
    const next = nextCronOccurrence("0 8 * * *", from, "America/New_York");
    expect(next!.toISOString()).toBe("2026-11-01T13:00:00.000Z");
  });

  it("the zone's date decides the day, even when UTC is already tomorrow", () => {
    // 2026-09-24 02:00 UTC is still Wed Sep 23, 22:00 in New York.
    const from = new Date(Date.UTC(2026, 8, 24, 2, 0, 0));
    const next = nextCronOccurrence("30 23 * * 3", from, "America/New_York");
    expect(next!.toISOString()).toBe("2026-09-24T03:30:00.000Z");
  });

  it("the browser's own DST never leaks into another zone's schedule", () => {
    // 02:30 on Sun Mar 8 2026 is a spring-forward gap in New York but a real
    // time in Tokyo (no DST). From 2026-03-07 12:00Z (21:00 Sat in Tokyo) the
    // next Tokyo Sunday 02:30 is Mar 8, whatever zone the browser is in.
    const from = new Date(Date.UTC(2026, 2, 7, 12, 0, 0));
    const next = nextCronOccurrence("30 2 * * 0", from, "Asia/Tokyo");
    expect(next!.toISOString()).toBe("2026-03-07T17:30:00.000Z");
    expect(formatNextRun(next!, "Asia/Tokyo")).toBe("Sun, Mar 8");
  });

  it("skips a wall-clock time the zone itself jumps over", () => {
    // New York has no 02:30 on Mar 8 2026; the next 02:30 is Mar 9.
    const from = new Date(Date.UTC(2026, 2, 7, 12, 0, 0));
    const next = nextCronOccurrence("30 2 * * *", from, "America/New_York");
    expect(next!.toISOString()).toBe("2026-03-09T06:30:00.000Z");
  });

  it("returns null for an unknown zone", () => {
    expect(nextCronOccurrence("0 8 * * *", NOON_UTC, "Mars/Olympus_Mons")).toBeNull();
  });
});
