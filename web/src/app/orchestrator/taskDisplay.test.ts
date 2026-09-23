import { describe, expect, it } from "vitest";

import type { Task } from "@/app/shared/lib/orchestratorApi";

import { scheduleLabel, scheduleStoppedReason, scheduleTitle } from "./taskDisplay";

const recurring: Task = { id: "t1", recurrence: "0 9 * * 6,0" };

describe("schedule display of a stopped schedule (ADR-0073)", () => {
  it("labels a parked recurring occurrence as stopped, with the reason on hover", () => {
    const parked: Task = {
      ...recurring,
      status: "dead_lettered",
      recurrence_parked_at: "2026-09-22T18:00:05Z",
      recurrence_parked_reason: "2 consecutive occurrences were dead-lettered. Replay this occurrence to resume the schedule.",
    };
    expect(scheduleLabel(parked)).toBe("⏹ Schedule stopped");
    expect(scheduleTitle(parked)).toBe(
      "Schedule stopped: 2 consecutive occurrences were dead-lettered. Replay this occurrence to resume the schedule.",
    );
  });

  it("falls back to a generic reason for an older parked row", () => {
    const parked: Task = { ...recurring, recurrence_parked_at: "2026-09-01T00:00:00Z", recurrence_parked_reason: null };
    expect(scheduleStoppedReason(parked)).toMatch(/Replay this run to resume it/);
  });

  it("leaves a running schedule and a one-off task alone", () => {
    expect(scheduleStoppedReason(recurring)).toBeNull();
    expect(scheduleTitle(recurring)).toBe("0 9 * * 6,0");
    expect(scheduleLabel(recurring)).toMatch(/^🔄 /);
    expect(scheduleStoppedReason({ id: "t2", recurrence_parked_at: "2026-09-01T00:00:00Z" })).toBeNull();
  });
});
