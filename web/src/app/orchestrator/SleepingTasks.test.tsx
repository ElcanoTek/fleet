import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { SleepingTasks, SLEEPING_LIMIT } from "./SleepingTasks";
import type { Task } from "@/app/shared/lib/orchestratorApi";

// SleepingTasks — the paused_awaiting_wake queue above the task table. It
// fetches its own slice of the list, so two things must hold: the count is
// the server's TOTAL (not the length of the first page), and it refetches on
// the dashboard's cadence via `refreshKey` instead of freezing at mount.

const tasks = vi.fn();
vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: {
    tasks: (...args: unknown[]) => tasks(...args),
  },
}));

afterEach(() => {
  cleanup();
  tasks.mockReset();
});

function sleeper(id: string): Task {
  return { id, prompt: `task ${id}`, status: "paused_awaiting_wake", wake_event_key: "deploy-done" } as Task;
}

describe("SleepingTasks", () => {
  it("renders nothing when no task is sleeping", async () => {
    tasks.mockResolvedValue({ data: [], total: 0 });
    const { container } = render(<SleepingTasks onOpen={() => {}} />);
    await waitFor(() => expect(tasks).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
  });

  it("counts the server total and says when the list is only the first page", async () => {
    tasks.mockResolvedValue({
      data: Array.from({ length: SLEEPING_LIMIT }, (_, i) => sleeper(`t${i}`)),
      total: 37,
    });
    render(<SleepingTasks onOpen={() => {}} />);
    expect(await screen.findByTestId("sleeping-tasks-count")).toHaveTextContent("37");
    expect(screen.getByTestId("sleeping-tasks-truncated")).toHaveTextContent(`showing the first ${SLEEPING_LIMIT}`);
    expect(tasks).toHaveBeenCalledWith(`status=paused_awaiting_wake&limit=${SLEEPING_LIMIT}&offset=0`);
  });

  it("omits the first-page note when everything fits", async () => {
    tasks.mockResolvedValue({ data: [sleeper("a"), sleeper("b")], total: 2 });
    render(<SleepingTasks onOpen={() => {}} />);
    expect(await screen.findByTestId("sleeping-tasks-count")).toHaveTextContent("2");
    expect(screen.queryByTestId("sleeping-tasks-truncated")).toBeNull();
  });

  it("refetches when refreshKey changes, so a task that woke leaves the queue", async () => {
    tasks.mockResolvedValue({ data: [sleeper("a")], total: 1 });
    const { rerender } = render(<SleepingTasks onOpen={() => {}} refreshKey={1} />);
    await screen.findByTestId("sleeping-task-row");
    expect(tasks).toHaveBeenCalledTimes(1);

    tasks.mockResolvedValue({ data: [], total: 0 });
    rerender(<SleepingTasks onOpen={() => {}} refreshKey={2} />);
    await waitFor(() => expect(tasks).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByTestId("sleeping-tasks")).toBeNull());
  });
});
