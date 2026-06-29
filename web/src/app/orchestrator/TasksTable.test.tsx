import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";
import { TasksTable, type TasksTableProps } from "./TasksTable";
import type { TaskFilters } from "@/app/shared/hooks/useDashboardData";
import type { Task } from "@/app/shared/lib/orchestratorApi";

// #126: the search box is debounced (~300ms) so a keystroke storm does not fire
// a dashboard refetch per character; the status select must stay INSTANT.

const FILTERS: TaskFilters = {
  status: "",
  query: "",
  scheduledOnly: false,
  completedToday: false,
  completedStatus: "",
  createdBy: "",
};

function renderTable(onFilters: TasksTableProps["onFilters"]) {
  return render(
    <TasksTable
      tasks={[]}
      total={0}
      page={1}
      pageSize={20}
      filters={FILTERS}
      onFilters={onFilters}
      onPage={() => {}}
      onPageSize={() => {}}
      onOpenLogs={() => {}}
    />,
  );
}

afterEach(() => cleanup());

describe("TasksTable search debounce", () => {
  it("does not call onFilters per keystroke, then fires once after the debounce", async () => {
    const onFilters = vi.fn();
    renderTable(onFilters);
    const input = screen.getByLabelText("Search tasks");

    fireEvent.change(input, { target: { value: "a" } });
    fireEvent.change(input, { target: { value: "ab" } });
    fireEvent.change(input, { target: { value: "abc" } });

    // No per-keystroke calls — the debounce timer hasn't elapsed yet.
    expect(onFilters).not.toHaveBeenCalled();

    // After the debounce settles, exactly one propagation with the final value.
    await waitFor(() => expect(onFilters).toHaveBeenCalledWith({ query: "abc" }));
    expect(onFilters).toHaveBeenCalledTimes(1);
  });

  it("keeps the status select instant (no debounce regression)", () => {
    const onFilters = vi.fn();
    renderTable(onFilters);
    fireEvent.change(screen.getByLabelText("Filter by status"), { target: { value: "running" } });
    // Synchronous — the select is not debounced.
    expect(onFilters).toHaveBeenCalledWith({ status: "running" });
  });
});

describe("TasksTable SLA badge (#274)", () => {
  const baseTask: Task = {
    id: "11111111-2222-3333-4444-555555555555",
    prompt: "daily-report",
    status: "success",
  };

  function renderWithTasks(tasks: Task[]) {
    return render(
      <TasksTable
        tasks={tasks}
        total={tasks.length}
        page={1}
        pageSize={20}
        filters={FILTERS}
        onFilters={() => {}}
        onPage={() => {}}
        onPageSize={() => {}}
        onOpenLogs={() => {}}
      />,
    );
  }

  it("renders no SLA badge when the task has no expected_duration_minutes", () => {
    renderWithTasks([baseTask]);
    expect(screen.queryByText(/SLA breached/)).toBeNull();
  });

  it("renders an SLA breached badge and tags the row when sla_breached is true", () => {
    const breached: Task = { ...baseTask, expected_duration_minutes: 15, sla_breached: true, actual_duration_seconds: 1200 };
    renderWithTasks([breached]);
    const badge = screen.getByText("SLA breached");
    expect(badge).toBeTruthy();
    // The row carrying the breach should expose the data attribute for
    // operator-facing styling / E2E selection.
    const tr = badge.closest("tr");
    expect(tr?.dataset.slaBreached).toBe("true");
  });

  it("renders the actual/expected ratio when an SLA is set but not breached", () => {
    const ok: Task = { ...baseTask, expected_duration_minutes: 20, actual_duration_seconds: 600 }; // 10m / 20m
    renderWithTasks([ok]);
    expect(screen.getByText("10m / 20m")).toBeTruthy();
  });
});
