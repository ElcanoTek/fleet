import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import { Checklist, parseTaskTrackerOutput, type ChecklistState } from "./Checklist";
import { LogViewer } from "./LogViewer";
import type { Task, TaskStreamFrame } from "@/app/shared/lib/orchestratorApi";

// Live checklist for scheduled runs (#518): the orchestrator parses the
// task_tracker tool result into a todo→done list and renders it live.

const streamTaskActivity = vi.fn();
vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: {
    streamTaskActivity: (...args: unknown[]) => streamTaskActivity(...args),
    cancelTask: vi.fn(),
  },
}));

beforeAll(() => {
  // jsdom has no scrollIntoView; LiveTaskView calls it on new activity.
  Element.prototype.scrollIntoView = vi.fn();
});

afterEach(() => cleanup());

const trackerOutput = JSON.stringify({
  status: "ok",
  command: "plan",
  tasks: [
    { id: "1", title: "Pull the data", status: "done" },
    { id: "2", title: "Analyze trends", status: "in_progress" },
    { id: "3", title: "Write the report", status: "todo" },
  ],
  summary: { total: 3, todo: 1, in_progress: 1, done: 1 },
  active_task: "Analyze trends",
});

describe("parseTaskTrackerOutput", () => {
  it("parses a task_tracker result into a checklist with a summary", () => {
    const state = parseTaskTrackerOutput(trackerOutput);
    expect(state).not.toBeNull();
    expect(state!.tasks).toHaveLength(3);
    expect(state!.summary).toEqual({ total: 3, todo: 1, in_progress: 1, done: 1 });
    expect(state!.activeTask).toBe("Analyze trends");
  });

  it("falls back to the input task_list when there is no result tasks array", () => {
    const raw = JSON.stringify({ task_list: [{ id: "1", title: "Only planned", status: "todo" }] });
    const state = parseTaskTrackerOutput(raw);
    expect(state).not.toBeNull();
    expect(state!.tasks[0].title).toBe("Only planned");
  });

  it("returns null for non-JSON, non-object, or empty task lists", () => {
    expect(parseTaskTrackerOutput("not json")).toBeNull();
    expect(parseTaskTrackerOutput('"a string"')).toBeNull();
    expect(parseTaskTrackerOutput(JSON.stringify({ tasks: [] }))).toBeNull();
    expect(parseTaskTrackerOutput(JSON.stringify({ tasks: [{ title: "", status: "todo" }] }))).toBeNull();
  });

  it("drops entries with an unrecognized status", () => {
    const raw = JSON.stringify({ tasks: [{ title: "ok", status: "todo" }, { title: "bad", status: "cancelled" }] });
    const state = parseTaskTrackerOutput(raw);
    expect(state!.tasks).toHaveLength(1);
  });
});

describe("Checklist component", () => {
  it("renders each task with a summary count", () => {
    const state = parseTaskTrackerOutput(trackerOutput) as ChecklistState;
    render(<Checklist state={state} />);
    expect(screen.getAllByTestId("checklist-item")).toHaveLength(3);
    expect(screen.getByTestId("checklist-summary").textContent).toContain("1/3 done");
  });
});

describe("LiveTaskView checklist wiring", () => {
  it("renders a live checklist when a task_tracker result streams in", async () => {
    let onFrame: ((f: TaskStreamFrame) => void) | null = null;
    streamTaskActivity.mockReset();
    streamTaskActivity.mockImplementation((_id: string, cb: (f: TaskStreamFrame) => void) => {
      onFrame = cb;
      return new Promise<void>(() => {}); // a live stream never resolves
    });

    const task = { id: "t1", status: "running" } as unknown as Task;
    render(<LogViewer task={task} onClose={() => {}} />);

    // Before any frame, no checklist.
    expect(screen.queryByTestId("live-checklist")).toBeNull();

    // A task_tracker tool_result arrives mid-run → the live checklist appears.
    act(() => {
      onFrame!({ type: "tool_result", name: "task_tracker", output: trackerOutput } as TaskStreamFrame);
    });

    expect(screen.getByTestId("live-checklist")).toBeTruthy();
    expect(screen.getByTestId("live-checklist").textContent).toContain("1/3 done");
    // The three plan items render (both in the live panel and the history entry).
    expect(screen.getAllByTestId("checklist-item").length).toBeGreaterThanOrEqual(3);
  });
});
