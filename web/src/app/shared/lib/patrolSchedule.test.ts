import { describe, expect, it } from "vitest";
import {
  buildPatrolCron,
  formatCompactCronCadence,
  MINUTE_INTERVAL_OPTIONS,
  parsePatrolCron,
} from "./patrolSchedule";

describe("patrolSchedule", () => {
  it("only offers minute intervals that divide 60 evenly", () => {
    for (const m of MINUTE_INTERVAL_OPTIONS) {
      expect(60 % m).toBe(0);
    }
  });

  it("round-trips simple hourly, minute, daily, weekday, and weekly schedules", () => {
    expect(
      buildPatrolCron({
        frequency: "minutes",
        intervalMinutes: 15,
        intervalHours: 2,
        time: "09:00",
        days: [1, 2, 3, 4, 5],
        rawCron: "",
      }),
    ).toBe("*/15 * * * *");

    expect(
      buildPatrolCron({
        frequency: "hours",
        intervalMinutes: 30,
        intervalHours: 1,
        time: "09:00",
        days: [1, 2, 3, 4, 5],
        rawCron: "",
      }),
    ).toBe("0 * * * *");

    expect(
      buildPatrolCron({
        frequency: "hours",
        intervalMinutes: 30,
        intervalHours: 4,
        time: "09:00",
        days: [1, 2, 3, 4, 5],
        rawCron: "",
      }),
    ).toBe("0 */4 * * *");

    expect(
      buildPatrolCron({
        frequency: "weekdays",
        intervalMinutes: 30,
        intervalHours: 2,
        time: "08:30",
        days: [1, 2, 3, 4, 5],
        rawCron: "",
      }),
    ).toBe("30 8 * * 1-5");

    expect(
      buildPatrolCron({
        frequency: "weekly",
        intervalMinutes: 30,
        intervalHours: 2,
        time: "14:15",
        days: [5, 1, 3],
        rawCron: "",
      }),
    ).toBe("15 14 * * 1,3,5");

    const allSevenCron = buildPatrolCron({
      frequency: "weekly",
      intervalMinutes: 30,
      intervalHours: 2,
      time: "09:00",
      days: [1, 2, 3, 4, 5, 6, 0],
      rawCron: "",
    });
    expect(parsePatrolCron(allSevenCron).days).toEqual([1, 2, 3, 4, 5, 6, 0]);
  });

  it("parses standard cron expressions into simple builder states and falls back to custom_cron for complex ones", () => {
    expect(parsePatrolCron("*/30 * * * *")).toMatchObject({
      frequency: "minutes",
      intervalMinutes: 30,
    });
    expect(parsePatrolCron("0 * * * *")).toMatchObject({
      frequency: "hours",
      intervalHours: 1,
    });
    expect(parsePatrolCron("0 */6 * * *")).toMatchObject({
      frequency: "hours",
      intervalHours: 6,
    });
    expect(parsePatrolCron("30 7 * * *")).toMatchObject({
      frequency: "daily",
      time: "07:30",
    });
    expect(parsePatrolCron("0 9 * * 1-5")).toMatchObject({
      frequency: "weekdays",
      time: "09:00",
    });
    expect(parsePatrolCron("0 16 * * 1,3,5")).toMatchObject({
      frequency: "weekly",
      time: "16:00",
      days: [1, 3, 5],
    });
    expect(parsePatrolCron("0 9,13,17 * * 1-5")).toMatchObject({
      frequency: "custom_cron",
      rawCron: "0 9,13,17 * * 1-5",
    });
  });

  it("formats compact human-readable cadences", () => {
    expect(formatCompactCronCadence("0 * * * *")).toBe("Every hour");
    expect(formatCompactCronCadence("0 */2 * * *")).toBe("Every 2h");
    expect(formatCompactCronCadence("*/15 * * * *")).toBe("Every 15m");
    expect(formatCompactCronCadence("0 9 * * *")).toBe("Daily 09:00");
    expect(formatCompactCronCadence("0 9 * * 1-5")).toBe("Weekdays 09:00");
  });
});
