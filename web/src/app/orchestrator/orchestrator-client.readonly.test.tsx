import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import type { LogSession, Task } from "@/app/shared/lib/orchestratorApi";
import { OrchestratorClient } from "./orchestrator-client";

// A readonly Operations Center user is refused every mutating request with 403
// ("insufficient role: a readonly user cannot modify resources"), so the page
// must not offer the controls that would only earn that 403: both New task
// buttons, the row actions (edit / run now / stop / delete), and in the task
// record Edit, Resubmit, Delete, feedback and Stop. Every other role keeps
// them; only an explicit "readonly" hides anything.

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

let sessionRole: string | undefined = "readonly";
vi.mock("@/app/shared/hooks/useOrchestratorSession", () => ({
  useOrchestratorSession: () => ({
    ready: true,
    signedIn: true,
    username: "viewer@example.com",
    role: sessionRole,
    noAccess: false,
    unreachable: false,
    logout: async () => {},
  }),
}));

vi.mock("@/app/shared/hooks/useDashboardData", () => ({
  useDashboardData: () => ({
    stats: null,
    tasks: [],
    total: 0,
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
    reload: async () => {},
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
// The table stub reports which row actions the page handed it: the real table
// renders an action only when its callback is present.
vi.mock("./TasksTable", () => ({
  TasksTable: (props: Record<string, unknown>) => (
    <div data-testid="tasks-table-actions">
      {["onEdit", "onRunNow", "onStop", "onDelete"]
        .filter((k) => typeof props[k] === "function")
        .join(",")}
    </div>
  ),
}));

const TASK_ID = "5b0c1f36-8f2c-4a39-9d43-7c1f0a2e6b11";
const FINISHED: Task = {
  id: TASK_ID,
  prompt: "Refresh the weekly pacing report",
  status: "success",
  created_by_username: "viewer@example.com",
};
const RUNNING: Task = { ...FINISHED, status: "running" };
const TRANSCRIPT = {
  id: "sess-1",
  messages: [{ id: "a1", role: "assistant", content: "Pacing report refreshed." }],
} as unknown as LogSession;

beforeAll(() => {
  Element.prototype.scrollIntoView = vi.fn();
});

beforeEach(() => {
  sessionRole = "readonly";
  task.mockReset();
  streamTaskActivity.mockReset();
  streamTaskActivity.mockImplementation(() => new Promise<void>(() => {}));
  taskLogs.mockReset();
  taskLogs.mockResolvedValue(TRANSCRIPT);
  taskLogHistory.mockReset();
  taskLogHistory.mockResolvedValue({ entries: [] });
});

afterEach(() => {
  cleanup();
  window.history.replaceState(null, "", "/");
});

function renderPage(linkedTask?: string) {
  window.history.replaceState(
    null,
    "",
    linkedTask ? `/orchestrator?task=${linkedTask}` : "/orchestrator",
  );
  render(<OrchestratorClient magicLinkLoginEnabled={false} />);
}

describe("Operations Center for a readonly user", () => {
  it("offers no New task button and no row actions", () => {
    renderPage();
    expect(screen.getByTestId("orchestrator-dashboard")).toBeTruthy();
    expect(screen.queryByTestId("new-task-btn")).toBeNull();
    expect(screen.queryByTestId("new-task-btn-topbar")).toBeNull();
    expect(screen.getByTestId("tasks-table-actions").textContent).toBe("");
  });

  it("keeps every control for a client", () => {
    sessionRole = "client";
    renderPage();
    expect(screen.getByTestId("new-task-btn")).toBeTruthy();
    expect(screen.getByTestId("new-task-btn-topbar")).toBeTruthy();
    expect(screen.getByTestId("tasks-table-actions").textContent).toBe(
      "onEdit,onRunNow,onStop,onDelete",
    );
  });

  it("keeps every control for a principal without a role (admin API key)", () => {
    sessionRole = undefined;
    renderPage();
    expect(screen.getByTestId("new-task-btn")).toBeTruthy();
    expect(screen.getByTestId("tasks-table-actions").textContent).toBe(
      "onEdit,onRunNow,onStop,onDelete",
    );
  });

  it("shows a finished record with no Edit, Resubmit, Delete or feedback", async () => {
    task.mockResolvedValue(FINISHED);
    renderPage(TASK_ID);
    const record = await screen.findByRole("dialog", { name: "Task Logs" });
    // Reads stay: the transcript, History, download.
    expect(await within(record).findByTestId("discuss-run-button")).toBeTruthy();
    expect(within(record).getByTestId("history-button")).toBeTruthy();
    expect(within(record).getByTestId("download-logs-button")).toBeTruthy();
    expect(within(record).queryByTestId("edit-task-button")).toBeNull();
    expect(within(record).queryByTestId("resubmit-task-button")).toBeNull();
    expect(within(record).queryByTestId("delete-task-button")).toBeNull();
    expect(within(record).queryByRole("button", { name: "Mark run as helpful" })).toBeNull();
    expect(within(record).queryByRole("button", { name: "Mark run as not helpful" })).toBeNull();
  });

  it("shows the same record with its actions for a client", async () => {
    sessionRole = "client";
    task.mockResolvedValue(FINISHED);
    renderPage(TASK_ID);
    const record = await screen.findByRole("dialog", { name: "Task Logs" });
    expect(await within(record).findByTestId("resubmit-task-button")).toBeTruthy();
    expect(within(record).getByTestId("delete-task-button")).toBeTruthy();
    expect(within(record).getByRole("button", { name: "Mark run as helpful" })).toBeTruthy();
  });

  it("offers no Stop on its own live run", async () => {
    task.mockResolvedValue(RUNNING);
    renderPage(TASK_ID);
    expect(await screen.findByRole("dialog", { name: "Live task activity" })).toBeTruthy();
    await waitFor(() => expect(streamTaskActivity).toHaveBeenCalled());
    expect(screen.queryByTestId("stop-task-button")).toBeNull();
  });

  it("offers Stop on a client's own live run", async () => {
    sessionRole = "client";
    task.mockResolvedValue(RUNNING);
    renderPage(TASK_ID);
    expect(await screen.findByRole("dialog", { name: "Live task activity" })).toBeTruthy();
    expect(screen.getByTestId("stop-task-button")).toBeTruthy();
  });
});
