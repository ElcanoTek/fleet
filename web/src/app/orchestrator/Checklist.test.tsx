import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { Checklist, parseTaskTrackerOutput, type ChecklistState } from "./Checklist";
import { LogViewer } from "./LogViewer";
import type { Task, TaskStreamFrame } from "@/app/shared/lib/orchestratorApi";

// Live checklist for scheduled runs (#518): the orchestrator parses the
// task_tracker tool result into a todo→done list and renders it live.

const streamTaskActivity = vi.fn();
const taskLogs = vi.fn();
vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: {
    streamTaskActivity: (...args: unknown[]) => streamTaskActivity(...args),
    taskLogs: (...args: unknown[]) => taskLogs(...args),
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

    const panel = screen.getByTestId("live-progress-panel");
    expect(panel.textContent).toContain("1/3 done");
    expect(panel.textContent).toContain("Analyze trends");
    // The full plan stays collapsed and never gets duplicated into history.
    expect(screen.queryByTestId("live-checklist")).toBeNull();
    expect(screen.queryAllByTestId("checklist-item")).toHaveLength(0);

    fireEvent.click(screen.getByRole("button", { name: /Progress/i }));
    expect(screen.getByTestId("live-checklist")).toBeTruthy();
    expect(screen.getAllByTestId("checklist-item")).toHaveLength(3);
  });

  it("pairs deferred calls with results and collapses repeated failures", () => {
    let onFrame: ((f: TaskStreamFrame) => void) | null = null;
    streamTaskActivity.mockReset();
    streamTaskActivity.mockImplementation((_id: string, cb: (f: TaskStreamFrame) => void) => {
      onFrame = cb;
      return new Promise<void>(() => {});
    });

    const task = { id: "t2", status: "running" } as unknown as Task;
    render(<LogViewer task={task} onClose={() => {}} />);
    const input = JSON.stringify({
      name: "mcp_fast_io_download",
      arguments: [{ action: "file-url", node_id: "node-1" }],
    });
    const failure = "invalid arguments: json: cannot unmarshal array into Go value of type map[string]interface {}";

    act(() => {
      onFrame!({ type: "tool_call", call_id: "c1", name: "tool_call", input } as TaskStreamFrame);
      onFrame!({ type: "tool_result", call_id: "c1", name: "tool_call", output: failure, error: true } as TaskStreamFrame);
      onFrame!({ type: "tool_call", call_id: "c2", name: "tool_call", input } as TaskStreamFrame);
      onFrame!({ type: "tool_result", call_id: "c2", name: "tool_call", output: failure, error: true } as TaskStreamFrame);
    });

    expect(screen.getAllByTestId("live-tool-entry")).toHaveLength(1);
    expect(screen.getByText("mcp_fast_io_download")).toBeTruthy();
    expect(screen.getByText("Failed · 2 attempts")).toBeTruthy();
    expect(screen.getByText("Invalid tool arguments: Fleet expected a JSON object but received an array.")).toBeTruthy();
  });
});

describe("stored task tool activity", () => {
  it("renders the nested tool name and a readable failure after the run ends", async () => {
    taskLogs.mockReset();
    taskLogs.mockResolvedValue({
      messages: [
        {
          role: "assistant",
          tool_calls: [{
            id: "c1",
            name: "tool_call",
            arguments: JSON.stringify({
              name: "mcp_fast_io_download",
              arguments: [{ action: "file-url", node_id: "node-1" }],
            }),
          }],
        },
        {
          role: "tool",
          tool_call_id: "c1",
          tool_name: "tool_call",
          is_error: true,
          content: "json: cannot unmarshal array into Go value of type map[string]interface {}",
        },
      ],
    });

    const task = { id: "t3", status: "failed" } as unknown as Task;
    render(<LogViewer task={task} onClose={() => {}} />);

    expect(await screen.findAllByText("mcp_fast_io_download")).toHaveLength(2);
    expect(screen.getByText("Invalid tool arguments: Fleet expected a JSON object but received an array.")).toBeTruthy();
  });

  it("reconnects a cleanly dropped stream from the last SSE event id", async () => {
    vi.useFakeTimers();
    try {
      streamTaskActivity.mockReset();
      streamTaskActivity
        .mockImplementationOnce((_id: string, cb: (f: TaskStreamFrame) => void) => {
          cb({ type: "agent_message", content: "started", _event_id: "17" });
          return Promise.resolve();
        })
        .mockImplementationOnce(() => new Promise<void>(() => {}));

      const task = { id: "t-reconnect", status: "running" } as unknown as Task;
      render(<LogViewer task={task} onClose={() => {}} />);
      await act(async () => { await Promise.resolve(); });
      await act(async () => { vi.advanceTimersByTime(500); await Promise.resolve(); });

      expect(streamTaskActivity).toHaveBeenCalledTimes(2);
      expect(streamTaskActivity.mock.calls[1][3]).toBe("17");
      expect(screen.getByText("started")).toBeTruthy();
    } finally {
      vi.useRealTimers();
    }
  });
});
