import { describe, expect, it } from "vitest";
import { dateInZone, endOfDayInZone, nextCronOccurrence, parseCronExpression, formatNextRun } from "./cronNext";

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

  it("fires the second 01:30 of a fall-back night, like the backend", () => {
    // 2026-11-01 05:45Z is 01:45 EDT; clocks then fall back to 01:00 EST, so
    // 01:30 EST (06:30Z) is still ahead.
    const from = new Date(Date.UTC(2026, 10, 1, 5, 45, 0));
    const next = nextCronOccurrence("30 1 * * *", from, "America/New_York");
    expect(next!.toISOString()).toBe("2026-11-01T06:30:00.000Z");
  });

  it("takes the first 01:30 of a fall-back night when both are ahead", () => {
    const from = new Date(Date.UTC(2026, 10, 1, 4, 0, 0)); // 00:00 EDT
    const next = nextCronOccurrence("30 1 * * *", from, "America/New_York");
    expect(next!.toISOString()).toBe("2026-11-01T05:30:00.000Z");
  });

  it("orders a fall-back day by instant, not wall clock", () => {
    // From 01:50 EDT (05:50Z): 01:45 EST (06:45Z) comes before 02:00 EST.
    const from = new Date(Date.UTC(2026, 10, 1, 5, 50, 0));
    const next = nextCronOccurrence("45 1,2 * * *", from, "America/New_York");
    expect(next!.toISOString()).toBe("2026-11-01T06:45:00.000Z");
  });

  it("takes the first of a repeated time even for a large offset (Auckland)", () => {
    // Pacific/Auckland falls back 03:00 NZDT → 02:00 NZST on 2026-04-05
    // (14:00Z Apr 4). The first 02:30 is 13:30Z under +13 — what cron.Next
    // returns — not the second at 14:30Z.
    const from = new Date(Date.UTC(2026, 3, 4, 12, 0, 0));
    const next = nextCronOccurrence("30 2 * * *", from, "Pacific/Auckland");
    expect(next!.toISOString()).toBe("2026-04-04T13:30:00.000Z");
  });

  it("an every-minute schedule is the next whole minute, cheaply", () => {
    const from = new Date(Date.UTC(2026, 8, 23, 12, 0, 30));
    const started = performance.now();
    for (let i = 0; i < 50; i++) {
      expect(nextCronOccurrence("* * * * *", from, "America/New_York")!.toISOString()).toBe(
        "2026-09-23T12:01:00.000Z",
      );
    }
    // Generous bound: the brute-force scan took far longer than this.
    expect(performance.now() - started).toBeLessThan(500);
  });

  it("a calendar day the zone skipped is not a match (Pacific/Apia, 2011-12-30)", () => {
    const from = new Date(Date.UTC(2011, 11, 29, 23, 0, 0)); // Dec 29 13:00 in Apia (UTC-10)
    // Dec 30 2011 never happened there, so "noon on the 30th" next falls on
    // Jan 30 2012 (UTC+14) — not on Dec 31, whose noon reads back as 12:00.
    const next = nextCronOccurrence("0 12 30 * *", from, "Pacific/Apia");
    expect(next!.toISOString()).toBe("2012-01-29T22:00:00.000Z");
  });

  it("a backward jump across midnight is ordered by instant (Antarctica/Casey, 2010)", () => {
    // +11 → +8 at 2010-03-05 00:00 local: from 13:00Z (00:00 +11), the next
    // minute is 13:01Z (00:01 +11), not the post-jump 23:00 +8 on Mar 4.
    const from = new Date(Date.UTC(2010, 2, 4, 13, 0, 0));
    const next = nextCronOccurrence("* * * * *", from, "Antarctica/Casey");
    expect(next!.toISOString()).toBe("2010-03-04T13:01:00.000Z");
  });

  it("returns null for an unknown zone", () => {
    expect(nextCronOccurrence("0 8 * * *", NOON_UTC, "Mars/Olympus_Mons")).toBeNull();
  });
});

describe("repeat end dates in a zone", () => {
  it("ends at 23:59:59 of that day on the zone's clock", () => {
    expect(endOfDayInZone("2026-07-31", "Asia/Tokyo")!.toISOString()).toBe("2026-07-31T14:59:59.000Z");
    expect(endOfDayInZone("2026-07-31", "America/Los_Angeles")!.toISOString()).toBe("2026-08-01T06:59:59.000Z");
    expect(endOfDayInZone("2026-07-31", "UTC")!.toISOString()).toBe("2026-07-31T23:59:59.000Z");
  });

  it("round-trips through dateInZone, including on a DST-change day", () => {
    for (const [date, zone] of [
      ["2026-11-01", "America/New_York"],
      ["2026-03-08", "America/New_York"],
      ["2026-04-05", "Pacific/Auckland"],
      ["2026-07-31", "Asia/Kolkata"],
    ]) {
      expect(dateInZone(endOfDayInZone(date, zone)!, zone)).toBe(date);
    }
  });

  it("rejects malformed input", () => {
    expect(endOfDayInZone("July 31", "UTC")).toBeNull();
    expect(endOfDayInZone("2026-07-31", "Mars/Olympus_Mons")).toBeNull();
    expect(dateInZone(new Date("nope"), "UTC")).toBeNull();
  });
});
