import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useSyncExternalStore, type ReactNode } from "react";
import type { LogSession, Task, TaskStreamFrame } from "@/app/shared/lib/orchestratorApi";
import { OrchestratorClient } from "./orchestrator-client";

// The Operations Center keeps a task's record in sync with its run. A record
// opened while the run is live shows the live view; when the run ends, the
// record has to swap to the finished transcript and its action strip. The
// board's copy of the task did that only for a row on the board's current
// page, so a task opened from a notification link (or from History) stayed in
// the live view for good once its run ended.

const task = vi.fn();
const streamTaskActivity = vi.fn();
const taskLogs = vi.fn();
const taskLogHistory = vi.fn();
vi.mock("@/app/shared/lib/orchestratorApi", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/app/shared/lib/orchestratorApi")>();
  return {
    ...actual,
    orchestratorApi: {
      task: (...args: unknown[]) => task(...args),
      streamTaskActivity: (...args: unknown[]) => streamTaskActivity(...args),
      taskLogs: (...args: unknown[]) => taskLogs(...args),
      taskLogHistory: (...args: unknown[]) => taskLogHistory(...args),
      cancelTask: vi.fn(),
    },
  };
});

vi.mock("@/app/shared/hooks/useOrchestratorSession", () => ({
  useOrchestratorSession: () => ({
    ready: true,
    signedIn: true,
    username: "operator",
    role: "admin",
    noAccess: false,
    unreachable: false,
    logout: async () => {},
  }),
}));

// The board: boardTasks is the current page of Recent Tasks. setBoard is a
// list load that succeeded (new rows, and a re-render). reload changes
// nothing, which is what useDashboardData does when its list request fails:
// it keeps the rows it has.
let boardTasks: Task[] = [];
const boardListeners = new Set<() => void>();
function subscribeBoard(listener: () => void) {
  boardListeners.add(listener);
  return () => {
    boardListeners.delete(listener);
  };
}
function setBoard(tasks: Task[]) {
  act(() => {
    boardTasks = tasks;
    boardListeners.forEach((listener) => listener());
  });
}
const reload = vi.fn(async () => {});
vi.mock("@/app/shared/hooks/useDashboardData", () => ({
  useDashboardData: () => ({
    stats: null,
    tasks: useSyncExternalStore(subscribeBoard, () => boardTasks),
    total: boardTasks.length,
    page: 1,
    pageSize: 20,
    filters: {},
    tagOptions: [],
    setFilters: () => {},
    clearFilters: () => {},
    setPage: () => {},
    setPageSize: () => {},
    loading: false,
    error: null,
    reload,
    refreshSeconds: 5,
    refreshNonce: 0,
  }),
}));

vi.mock("@/app/shared/hooks/useMcpServers", () => ({
  useMcpServers: () => ({ servers: [], loading: false }),
}));
vi.mock("@/app/lib/useClientConfig", () => ({
  useClientConfig: () => ({ branding: { app_name: "Fleet", logo_url: "" } }),
  refreshClientConfig: async () => true,
}));

// The shell and the board's own panels fetch for themselves; none of that is
// under test here. The table stub opens a row the way the real one does.
vi.mock("@/app/shared/ui/NavRail", () => ({
  NavRail: ({ children }: { children: ReactNode }) => <nav>{children}</nav>,
  useRailCollapse: () => ({
    collapsed: false,
    isNarrow: false,
    railReady: true,
    setCollapsed: () => {},
  }),
}));
vi.mock("./ServerClock", () => ({ ServerClock: () => null }));
vi.mock("./SleepingTasks", () => ({ SleepingTasks: () => null }));
vi.mock("./TaskCreateModal", () => ({ TaskCreateModal: () => null }));
vi.mock("./TasksTable", () => ({
  TasksTable: ({ tasks, onOpenLogs }: { tasks: Task[]; onOpenLogs: (t: Task) => void }) => (
    <div id="tasksTable">
      {tasks.map((t) => (
        <button key={t.id} type="button" onClick={() => onOpenLogs(t)}>
          {t.prompt}
        </button>
      ))}
    </div>
  ),
}));

const TASK_ID = "5b0c1f36-8f2c-4a39-9d43-7c1f0a2e6b11";
const RUNNING: Task = {
  id: TASK_ID,
  prompt: "Refresh the weekly pacing report",
  status: "running",
};
const FINISHED: Task = { ...RUNNING, status: "success" };
const TRANSCRIPT = {
  id: "sess-1",
  messages: [
    { id: "u1", role: "user", content: "Refresh the weekly pacing report" },
    { id: "a1", role: "assistant", content: "Pacing report refreshed." },
  ],
} as unknown as LogSession;

// sendFrame delivers a frame on the live view's open stream.
let sendFrame: (frame: TaskStreamFrame) => void = () => {};

beforeAll(() => {
  // jsdom has no scrollIntoView; the live view calls it on new activity.
  Element.prototype.scrollIntoView = vi.fn();
});

beforeEach(() => {
  boardTasks = [];
  reload.mockClear();
  task.mockReset();
  sendFrame = () => {};
  streamTaskActivity.mockReset();
  streamTaskActivity.mockImplementation((_id: string, onFrame: (f: TaskStreamFrame) => void) => {
    sendFrame = onFrame;
    return new Promise<void>(() => {}); // a live stream stays open
  });
  taskLogs.mockReset();
  taskLogs.mockResolvedValue(TRANSCRIPT);
  taskLogHistory.mockReset();
  taskLogHistory.mockResolvedValue({ entries: [] });
});

afterEach(() => {
  cleanup();
  window.history.replaceState(null, "", "/");
});

function openLink(id: string) {
  window.history.replaceState(null, "", `/orchestrator?task=${id}`);
  render(<OrchestratorClient magicLinkLoginEnabled={false} />);
}

// The live view opens its stream in an effect, which a loaded runner can run
// after the dialog is already on screen; a frame sent before then reaches the
// no-op and is lost, so wait for the stream first.
async function endRun(status = "succeeded") {
  await waitFor(() => expect(streamTaskActivity).toHaveBeenCalled());
  act(() => {
    sendFrame({ type: "status", status } as TaskStreamFrame);
  });
}

describe("a record open when its live run ends", () => {
  it("swaps a linked task the board's page does not list to its finished record", async () => {
    task.mockResolvedValueOnce(RUNNING).mockResolvedValueOnce(FINISHED);
    openLink(TASK_ID);

    expect(await screen.findByRole("dialog", { name: "Live task activity" })).toBeTruthy();
    expect(screen.queryByText(RUNNING.prompt!, { selector: "#tasksTable *" })).toBeNull();

    await endRun();

    // The finished record: its transcript loaded (Discuss in chat shows only
    // for a transcript with messages) and its action strip.
    const record = await screen.findByRole("dialog", { name: "Task Logs" });
    expect(await within(record).findByTestId("discuss-run-button")).toBeTruthy();
    expect(within(record).getByTestId("resubmit-task-button")).toBeTruthy();
    expect(within(record).getByTestId("history-button")).toBeTruthy();
    expect(screen.queryByRole("dialog", { name: "Live task activity" })).toBeNull();
    expect(task).toHaveBeenCalledTimes(2);
    expect(task).toHaveBeenLastCalledWith(TASK_ID);
    expect(taskLogs).toHaveBeenCalledWith(TASK_ID);
  });

  it("swaps a row that left the board's page while it ran", async () => {
    boardTasks = [RUNNING];
    task.mockResolvedValueOnce(FINISHED);
    render(<OrchestratorClient magicLinkLoginEnabled={false} />);
    fireEvent.click(await screen.findByText(RUNNING.prompt!, { selector: "#tasksTable *" }));
    expect(await screen.findByRole("dialog", { name: "Live task activity" })).toBeTruthy();

    // Newer tasks pushed the row off the page before the run ended.
    setBoard([]);
    await endRun("failed");

    expect(await screen.findByRole("dialog", { name: "Task Logs" })).toBeTruthy();
    expect(task).toHaveBeenCalledWith(TASK_ID);
    // The board catches up now too, not at its next poll.
    expect(reload).toHaveBeenCalled();
  });

  // The board's copy of a listed task wins over the record, except the copy
  // from before the run ended: while the board cannot load its list again, it
  // keeps that row, which still reads "running".
  it("swaps a listed row while the board cannot reload its list", async () => {
    boardTasks = [RUNNING];
    task.mockResolvedValueOnce(FINISHED);
    render(<OrchestratorClient magicLinkLoginEnabled={false} />);
    fireEvent.click(await screen.findByText(RUNNING.prompt!, { selector: "#tasksTable *" }));
    await screen.findByRole("dialog", { name: "Live task activity" });

    await endRun();

    expect(await screen.findByRole("dialog", { name: "Task Logs" })).toBeTruthy();
    expect(reload).toHaveBeenCalled();
    expect(screen.queryByRole("dialog", { name: "Live task activity" })).toBeNull();
  });

  // Rows the board loads after the end win again, so a task that runs again
  // under the same id (a retry of a failed attempt, a wake) goes live.
  it("goes live again when the board later lists the task running again", async () => {
    boardTasks = [RUNNING];
    task.mockResolvedValueOnce({ ...RUNNING, status: "pending" });
    render(<OrchestratorClient magicLinkLoginEnabled={false} />);
    fireEvent.click(await screen.findByText(RUNNING.prompt!, { selector: "#tasksTable *" }));
    await screen.findByRole("dialog", { name: "Live task activity" });
    await endRun("failed");
    await screen.findByRole("dialog", { name: "Task Logs" });

    setBoard([{ ...RUNNING }]);

    expect(await screen.findByRole("dialog", { name: "Live task activity" })).toBeTruthy();
    expect(screen.queryByRole("dialog", { name: "Task Logs" })).toBeNull();
    expect(streamTaskActivity).toHaveBeenCalledTimes(2);
  });

  it("does not reopen a record closed before the fetch answers", async () => {
    let answer: (t: Task) => void = () => {};
    task
      .mockResolvedValueOnce(RUNNING)
      .mockReturnValueOnce(new Promise<Task>((resolve) => (answer = resolve)));
    openLink(TASK_ID);
    const live = await screen.findByRole("dialog", { name: "Live task activity" });

    await endRun();
    fireEvent.click(within(live).getByRole("button", { name: "Close modal" }));
    await act(async () => answer(FINISHED));

    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("keeps the ended live view when the task cannot be fetched again", async () => {
    task.mockResolvedValueOnce(RUNNING).mockRejectedValueOnce(new TypeError("Failed to fetch"));
    openLink(TASK_ID);
    await screen.findByRole("dialog", { name: "Live task activity" });

    await endRun();
    await act(async () => {});

    const live = screen.getByRole("dialog", { name: "Live task activity" });
    expect(within(live).getByTestId("live-run-status").textContent).toBe("succeeded");
    expect(task).toHaveBeenCalledTimes(2);
  });
});
