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
