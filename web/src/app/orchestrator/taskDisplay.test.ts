import { describe, expect, it } from "vitest";

import type { Task } from "@/app/shared/lib/orchestratorApi";

import { blockedReason, promptName, scheduleLabel, scheduleStoppedReason, scheduleTitle, taskRunLabel } from "./taskDisplay";

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
    expect(scheduleStoppedReason(parked)).toMatch(/corrected prompt/);
  });

  it("leaves a running schedule and a one-off task alone", () => {
    expect(scheduleStoppedReason(recurring)).toBeNull();
    expect(scheduleTitle(recurring)).toBe("0 9 * * 6,0");
    expect(scheduleLabel(recurring)).toMatch(/^🔄 /);
    expect(scheduleStoppedReason({ id: "t2", recurrence_parked_at: "2026-09-01T00:00:00Z" })).toBeNull();
  });
});

describe("blocked runs (completion.blocked_when)", () => {
  it("reads a success with the blocked outcome as Blocked, with its detail", () => {
    const task: Task = {
      id: "t3",
      status: "success",
      run_outcome: "blocked",
      run_outcome_detail: "outcome=failed: the export had zero rows",
    };
    expect(blockedReason(task)).toBe("outcome=failed: the export had zero rows");
  });

  it("falls back to a generic reason when no detail was recorded", () => {
    expect(blockedReason({ id: "t4", status: "success", run_outcome: "blocked" })).toMatch(/without publishing/);
  });

  it("leaves ordinary successes and other outcomes alone", () => {
    expect(blockedReason({ id: "t5", status: "success" })).toBeNull();
    expect(blockedReason({ id: "t6", status: "dead_lettered", run_outcome: "connector_unavailable" })).toBeNull();
    expect(blockedReason({ id: "t7", status: "error", run_outcome: "blocked" })).toBeNull();
  });
});

describe("task labels from a workflow's own name", () => {
  // Scheduled prompts often open with a shared preamble heading, so the first
  // line names every one of them the same; the YAML name: tells them apart.
  const prompt = '## Unattended run contract (read first)\n\nNo human is present.\n\n---\nname: "TWC Campaign Health Scan"\nversion: "4.3"\n';

  it("reads a top-level name: line, quoted or not", () => {
    expect(promptName(prompt)).toBe("TWC Campaign Health Scan");
    expect(promptName("name: Weekly scan\nsteps: []")).toBe("Weekly scan");
    expect(promptName("agent:\n  name: Victoria\n")).toBe("");
    expect(promptName("Pull yesterday's deal report")).toBe("");
  });

  it("labels an untitled task by it, after an explicit title", () => {
    expect(taskRunLabel({ id: "b50b6d78-0000", prompt } as Task)).toBe("TWC Campaign Health Scan");
    expect(taskRunLabel({ id: "b50b6d78-0000", prompt, title: "TWC weekly" } as Task)).toBe("TWC weekly");
    expect(taskRunLabel({ id: "b50b6d78-0000", prompt: "Run the optimization protocol" } as Task)).toBe("Run the optimization protocol");
  });
});
