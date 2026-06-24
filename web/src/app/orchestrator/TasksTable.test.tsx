import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";
import { TasksTable, type TasksTableProps } from "./TasksTable";
import type { TaskFilters } from "@/app/shared/hooks/useDashboardData";

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
