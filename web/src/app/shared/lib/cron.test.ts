import { describe, expect, it } from "vitest";
import { describeCronExpression } from "./cron";

// Exercises the cron → English describer ported from moc's utils.js. Covers the
// common preset shapes the task form's recurrence presets emit plus edge cases.

describe("describeCronExpression", () => {
  it("returns empty for invalid / unparseable input", () => {
    expect(describeCronExpression("")).toBe("");
    expect(describeCronExpression(null)).toBe("");
    expect(describeCronExpression("0 9 * *")).toBe(""); // too few fields
    expect(describeCronExpression("0 9 * * * * *")).toBe(""); // too many fields
  });

  it("describes a fixed daily time", () => {
    expect(describeCronExpression("0 9 * * *")).toBe("At 09:00");
  });

  it("describes weekday ranges (the 'Weekdays 9am' preset)", () => {
    expect(describeCronExpression("0 9 * * 1-5")).toBe("At 09:00, Monday through Friday");
  });

  it("describes a single weekday (the 'Weekly Mon' preset)", () => {
    expect(describeCronExpression("0 9 * * 1")).toBe("At 09:00, only on Monday");
  });

  it("describes a comma list of weekdays (the 'Mon & Thu 1pm' preset)", () => {
    expect(describeCronExpression("0 13 * * 1,4")).toBe("At 13:00, only on Monday and Thursday");
  });

  it("describes every-N-minutes", () => {
    expect(describeCronExpression("*/15 * * * *")).toBe("Every 15 minutes");
  });

  it("describes day-of-month", () => {
    expect(describeCronExpression("0 9 1 * *")).toBe("At 09:00, on the 1st of the month");
  });

  it("describes a month restriction", () => {
    expect(describeCronExpression("0 9 1 1 *")).toBe("At 09:00, on the 1st of the month, in January");
  });

  it("treats dom AND dow as an OR when both restricted", () => {
    expect(describeCronExpression("0 9 1 * 1")).toBe(
      "At 09:00, on the 1st of the month or only on Monday",
    );
  });
});
