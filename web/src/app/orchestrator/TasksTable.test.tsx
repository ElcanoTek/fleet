import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent, waitFor, within, cleanup } from "@testing-library/react";
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
  tags: [],
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
    // The badge renders in both the desktop table row and the phone card
    // (CSS decides which is visible).
    const badges = screen.getAllByText("SLA breached");
    expect(badges.length).toBe(2);
    // The row carrying the breach should expose the data attribute for
    // operator-facing styling / E2E selection.
    const tr = badges.map((b) => b.closest("tr")).find(Boolean);
    expect(tr?.dataset.slaBreached).toBe("true");
  });

  it("renders the actual/expected ratio when an SLA is set but not breached", () => {
    const ok: Task = { ...baseTask, expected_duration_minutes: 20, actual_duration_seconds: 600 }; // 10m / 20m
    renderWithTasks([ok]);
    expect(screen.getAllByText("10m / 20m").length).toBe(2);
  });

  it("describes a recurrence in plain English instead of raw cron", () => {
    const recurring: Task = { ...baseTask, recurrence: "0 9 * * 6,0" };
    renderWithTasks([recurring]);
    // Rendered in both the table row and the phone card; raw cron only in title.
    const labels = screen.getAllByText(/9:00 AM · Sat, Sun/);
    expect(labels.length).toBe(2);
    expect(screen.queryByText(/0 9 \* \* 6,0/)).toBeNull();
  });

  it("phone cards click through to the log viewer", () => {
    const onOpenLogs = vi.fn();
    render(
      <TasksTable
        tasks={[baseTask]}
        total={1}
        page={1}
        pageSize={20}
        filters={FILTERS}
        onFilters={() => {}}
        onPage={() => {}}
        onPageSize={() => {}}
        onOpenLogs={onOpenLogs}
      />,
    );
    const cards = screen.getByTestId("task-cards");
    fireEvent.click(within(cards).getByRole("button", { name: /view task/i }));
    expect(onOpenLogs).toHaveBeenCalledWith(baseTask);
  });
});

describe("TasksTable Run now action (#1019)", () => {
  const scheduled: Task = {
    id: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
    prompt: "Reklaim daily health scan",
    status: "scheduled",
    recurrence: "0 9 * * *",
  };

  function renderWithRunNow(tasks: Task[], onRunNow?: (t: Task) => void) {
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
        onRunNow={onRunNow}
      />,
    );
  }

  it("offers Run now on a scheduled task that has not run yet", () => {
    const onRunNow = vi.fn();
    renderWithRunNow([scheduled], onRunNow);
    fireEvent.click(screen.getByTestId("task-run-now-button"));
    expect(onRunNow).toHaveBeenCalledWith(scheduled);
  });

  it("does not open the log viewer when Run now is clicked (row click is suppressed)", () => {
    const onOpenLogs = vi.fn();
    render(
      <TasksTable
        tasks={[scheduled]}
        total={1}
        page={1}
        pageSize={20}
        filters={FILTERS}
        onFilters={() => {}}
        onPage={() => {}}
        onPageSize={() => {}}
        onOpenLogs={onOpenLogs}
        onRunNow={() => {}}
      />,
    );
    fireEvent.click(screen.getByTestId("task-run-now-button"));
    expect(onOpenLogs).not.toHaveBeenCalled();
  });

  it("hides Run now for an in-flight task", () => {
    renderWithRunNow([{ ...scheduled, status: "running" }], vi.fn());
    expect(screen.queryByTestId("task-run-now-button")).toBeNull();
    expect(screen.queryByTestId("task-run-now-button-card")).toBeNull();
  });

  it("omits the action entirely when the parent passes no handler", () => {
    renderWithRunNow([scheduled]);
    expect(screen.queryByTestId("task-run-now-button")).toBeNull();
  });

  it("fires from the phone card view too", () => {
    const onRunNow = vi.fn();
    renderWithRunNow([scheduled], onRunNow);
    const cards = screen.getByTestId("task-cards");
    fireEvent.click(within(cards).getByTestId("task-run-now-button-card"));
    expect(onRunNow).toHaveBeenCalledWith(scheduled);
  });
});

// Stopping was reachable only from inside the Live-activity modal, so a
// RECURRING job that had not fired yet could not be stopped from anywhere in
// this UI — even though DELETE /tasks/{id} has always supported it. "Also you
// can not delete a job in the operation center", 2026-08-13 (#1152).
describe("TasksTable Stop action (#1152)", () => {
  const scheduledJob: Task = {
    id: "aaaaaaaa-bbbb-cccc-dddd-ffffffffffff",
    prompt: "Comfluence daily refresh",
    status: "scheduled",
    recurrence: "0 12 * * *",
  };

  function renderWithStop(tasks: Task[], onStop?: (t: Task) => void, onOpenLogs = () => {}) {
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
        onOpenLogs={onOpenLogs}
        onStop={onStop}
      />,
    );
  }

  it("offers Stop on a recurring job that has not run yet — the case with no other route", () => {
    const onStop = vi.fn();
    renderWithStop([scheduledJob], onStop);
    fireEvent.click(screen.getByTestId("task-stop-button"));
    expect(onStop).toHaveBeenCalledWith(scheduledJob);
  });

  it("offers Stop on a live run too, so the list is not a dead end", () => {
    const onStop = vi.fn();
    renderWithStop([{ ...scheduledJob, status: "running" }], onStop);
    fireEvent.click(screen.getByTestId("task-stop-button"));
    expect(onStop).toHaveBeenCalled();
  });

  it("hides Stop on a task that already finished — there is nothing to stop", () => {
    renderWithStop([{ ...scheduledJob, status: "success" }], vi.fn());
    expect(screen.queryByTestId("task-stop-button")).toBeNull();
    expect(screen.queryByTestId("task-stop-button-card")).toBeNull();
  });

  it("says a recurring job will not run again, so the consequence is on the button", () => {
    renderWithStop([scheduledJob], vi.fn());
    expect(screen.getByTestId("task-stop-button").getAttribute("title")).toMatch(/not run again/i);
  });

  it("does not open the log viewer when Stop is clicked", () => {
    const onOpenLogs = vi.fn();
    renderWithStop([scheduledJob], () => {}, onOpenLogs);
    fireEvent.click(screen.getByTestId("task-stop-button"));
    expect(onOpenLogs).not.toHaveBeenCalled();
  });

  it("omits the action entirely when the parent passes no handler", () => {
    renderWithStop([scheduledJob]);
    expect(screen.queryByTestId("task-stop-button")).toBeNull();
  });

  it("fires from the phone card view too", () => {
    const onStop = vi.fn();
    renderWithStop([scheduledJob], onStop);
    const cards = screen.getByTestId("task-cards");
    fireEvent.click(within(cards).getByTestId("task-stop-button-card"));
    expect(onStop).toHaveBeenCalledWith(scheduledJob);
  });
});

// Cancelling keeps the row, and the row keeps its NAME — which is uniquely
// indexed — so a broken job blocked its own replacement, and could not even be
// renamed (editing is pending/scheduled only). Deleting was the only way out
// and fleet had it nowhere. "I can't make new ones if the old ones that don't
// work are still there."
describe("TasksTable Delete action", () => {
  const brokenJob: Task = {
    id: "aaaaaaaa-bbbb-cccc-dddd-111111111111",
    prompt: "Comfluence daily refresh",
    status: "error",
    recurrence: "0 12 * * *",
  };

  function renderWithDelete(tasks: Task[], onDelete?: (t: Task) => void, onOpenLogs = () => {}) {
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
        onOpenLogs={onOpenLogs}
        onDelete={onDelete}
      />,
    );
  }

  it("offers Delete on the broken job someone is trying to clear", () => {
    const onDelete = vi.fn();
    renderWithDelete([brokenJob], onDelete);
    fireEvent.click(screen.getByTestId("task-delete-button"));
    expect(onDelete).toHaveBeenCalledWith(brokenJob);
  });

  it("says what deleting buys, not just that it deletes", () => {
    renderWithDelete([brokenJob], vi.fn());
    expect(screen.getByTestId("task-delete-button").getAttribute("title")).toMatch(/frees its name/i);
  });

  // The server refuses a live run (the worker still holds the lease), so the
  // affordance is hidden rather than offered and then rejected.
  it("hides Delete on a live run", () => {
    for (const status of ["running", "leased"]) {
      const { unmount } = renderWithDelete([{ ...brokenJob, status }], vi.fn());
      expect(screen.queryByTestId("task-delete-button")).toBeNull();
      expect(screen.queryByTestId("task-delete-button-card")).toBeNull();
      unmount();
    }
  });

  it("offers Delete on a scheduled job that has not run yet", () => {
    const onDelete = vi.fn();
    renderWithDelete([{ ...brokenJob, status: "scheduled" }], onDelete);
    fireEvent.click(screen.getByTestId("task-delete-button"));
    expect(onDelete).toHaveBeenCalled();
  });

  it("does not open the log viewer when Delete is clicked", () => {
    const onOpenLogs = vi.fn();
    renderWithDelete([brokenJob], () => {}, onOpenLogs);
    fireEvent.click(screen.getByTestId("task-delete-button"));
    expect(onOpenLogs).not.toHaveBeenCalled();
  });

  it("omits the action entirely when the parent passes no handler", () => {
    renderWithDelete([brokenJob]);
    expect(screen.queryByTestId("task-delete-button")).toBeNull();
  });

  it("fires from the phone card view too", () => {
    const onDelete = vi.fn();
    renderWithDelete([brokenJob], onDelete);
    const cards = screen.getByTestId("task-cards");
    fireEvent.click(within(cards).getByTestId("task-delete-button-card"));
    expect(onDelete).toHaveBeenCalledWith(brokenJob);
  });
});

describe("TasksTable titles", () => {
  const base: Task = {
    id: "dddddddd-eeee-ffff-0000-111111111111",
    prompt: "Pull yesterday's numbers and email the pacing summary",
    status: "success",
  };

  function renderTasks(tasks: Task[]) {
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

  it("leads with the title and keeps the prompt as the secondary line", () => {
    renderTasks([{ ...base, title: "Daily pacing summary" }]);
    // Rendered in both the desktop row and the phone card (CSS picks one).
    expect(screen.getAllByText("Daily pacing summary").length).toBe(2);
    expect(
      screen.getAllByText(/Pull yesterday's numbers/).length,
    ).toBeGreaterThan(0);
  });

  it("falls back to the prompt alone when the task is untitled", () => {
    renderTasks([base]);
    expect(screen.getAllByText(/Pull yesterday's numbers/).length).toBe(2);
    expect(document.querySelector(".task-title-line")).toBeNull();
    expect(document.querySelector(".task-card-title")).toBeNull();
  });

  it("treats a whitespace-only title as untitled", () => {
    renderTasks([{ ...base, title: "   " }]);
    expect(document.querySelector(".task-title-line")).toBeNull();
  });
});

describe("TasksTable zero-row states", () => {
  function renderEmpty(extra: Partial<TasksTableProps>) {
    return render(
      <TasksTable
        tasks={[]}
        total={0}
        page={1}
        pageSize={20}
        filters={FILTERS}
        onFilters={() => {}}
        onPage={() => {}}
        onPageSize={() => {}}
        onOpenLogs={() => {}}
        {...extra}
      />,
    );
  }

  it("shows a loading line during the first fetch, not the empty message", () => {
    renderEmpty({ loading: true });
    expect(screen.getAllByTestId("tasks-loading").length).toBeGreaterThan(0);
    expect(screen.queryByText("No tasks created yet")).toBeNull();
  });

  it("shows the load error with a working Retry instead of 'No tasks created yet'", () => {
    const onRetry = vi.fn();
    renderEmpty({ error: "HTTP 503", onRetry });
    const alerts = screen.getAllByTestId("tasks-load-error");
    expect(alerts[0]).toHaveTextContent("Couldn't load tasks: HTTP 503");
    expect(screen.queryByText("No tasks created yet")).toBeNull();
    fireEvent.click(within(alerts[0]).getByRole("button", { name: "Retry" }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  it("keeps the genuine empty message when loaded with no error", () => {
    renderEmpty({});
    expect(screen.getAllByText("No tasks created yet").length).toBeGreaterThan(0);
  });
});

describe("TasksTable Clear filters", () => {
  function renderFiltered(filters: TaskFilters, extra: Partial<TasksTableProps> = {}) {
    return render(
      <TasksTable
        tasks={[]}
        total={0}
        page={1}
        pageSize={20}
        filters={filters}
        onFilters={() => {}}
        onPage={() => {}}
        onPageSize={() => {}}
        onOpenLogs={() => {}}
        {...extra}
      />,
    );
  }

  it("is hidden while no filter is set", () => {
    renderFiltered(FILTERS, { onClearFilters: () => {} });
    expect(screen.queryByTestId("tasks-clear-filters")).toBeNull();
  });

  it("appears once a filter is set and resets everything through the parent", () => {
    const onClearFilters = vi.fn();
    renderFiltered({ ...FILTERS, status: "running" }, { onClearFilters });
    // The search draft typed inside the debounce window is dropped too.
    fireEvent.change(screen.getByLabelText("Search tasks"), { target: { value: "half-typ" } });
    fireEvent.click(screen.getByTestId("tasks-clear-filters"));
    expect(onClearFilters).toHaveBeenCalledTimes(1);
    expect(screen.getByLabelText("Search tasks")).toHaveValue("");
  });

  it("is omitted entirely when the parent does not wire it", () => {
    renderFiltered({ ...FILTERS, createdBy: "me" });
    expect(screen.queryByTestId("tasks-clear-filters")).toBeNull();
  });
});

// ── Tag filter and tag chips (#212) ──────────────────────────────────────
// Tags were write-only: the create form accepted them, the API stored them,
// and no surface on the board ever showed one again — so the thing a tag is
// for, finding the rest of its group, could not be done from the UI at all.
describe("TasksTable tags", () => {
  const tagged: Task = {
    id: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
    prompt: "nightly reconciliation",
    status: "success",
    tags: ["ops", "billing"],
  };

  function renderTags(
    overrides: Partial<TasksTableProps> & { filters?: TaskFilters } = {},
  ) {
    const onFilters = vi.fn();
    render(
      <TasksTable
        tasks={[tagged]}
        total={1}
        page={1}
        pageSize={20}
        filters={FILTERS}
        tagOptions={["ops", "billing", "urgent"]}
        onFilters={onFilters}
        onPage={() => {}}
        onPageSize={() => {}}
        onOpenLogs={() => {}}
        {...overrides}
      />,
    );
    return onFilters;
  }

  it("shows a task's tags on the board", () => {
    renderTags();
    // Table row and phone card both render them (CSS picks one).
    expect(screen.getAllByLabelText("Filter by tag ops").length).toBe(2);
    expect(screen.getAllByLabelText("Filter by tag billing").length).toBe(2);
  });

  it("filters by a tag when its chip is clicked", () => {
    const onFilters = renderTags();
    fireEvent.click(screen.getAllByLabelText("Filter by tag ops")[0]);
    expect(onFilters).toHaveBeenCalledWith({ tags: ["ops"] });
  });

  it("does not open the log viewer when a chip is clicked", () => {
    // The row is itself a button; without stopPropagation, filtering by a tag
    // would also throw the log modal open over the board you just narrowed.
    const onOpenLogs = vi.fn();
    renderTags({ onOpenLogs });
    fireEvent.click(screen.getAllByLabelText("Filter by tag ops")[0]);
    expect(onOpenLogs).not.toHaveBeenCalled();
  });

  it("adds to the selection rather than replacing it — tags AND together", () => {
    const onFilters = renderTags({ filters: { ...FILTERS, tags: ["billing"] } });
    fireEvent.click(screen.getAllByLabelText("Filter by tag ops")[0]);
    expect(onFilters).toHaveBeenCalledWith({ tags: ["billing", "ops"] });
  });

  it("removes a tag when its selected chip is clicked again", () => {
    const onFilters = renderTags({ filters: { ...FILTERS, tags: ["ops", "billing"] } });
    fireEvent.click(screen.getAllByLabelText("Stop filtering by tag ops")[0]);
    expect(onFilters).toHaveBeenCalledWith({ tags: ["billing"] });
  });

  it("adds a tag chosen from the select", () => {
    const onFilters = renderTags();
    fireEvent.change(screen.getByLabelText("Filter by tag"), { target: { value: "urgent" } });
    expect(onFilters).toHaveBeenCalledWith({ tags: ["urgent"] });
  });

  it("keeps the select on its placeholder — it adds, it does not hold a value", () => {
    // Holding the last-added tag would claim the board is filtered by that one
    // tag when it is filtered by every chip beside it.
    renderTags({ filters: { ...FILTERS, tags: ["ops"] } });
    const select = screen.getByLabelText("Filter by tag") as HTMLSelectElement;
    expect(select.value).toBe("");
    // An already-selected tag is not offered twice.
    const offered = [...select.options].map((o) => o.value);
    expect(offered).not.toContain("ops");
    expect(offered).toContain("urgent");
  });

  it("hides the control entirely when nothing is tagged", () => {
    renderTags({ tasks: [], tagOptions: [] });
    expect(screen.queryByLabelText("Filter by tag")).toBeNull();
  });

  it("still shows the control when a tag is selected but the catalogue is empty", () => {
    // A failed catalogue fetch must not strand an applied filter with no way
    // to remove it.
    renderTags({ tasks: [], tagOptions: [], filters: { ...FILTERS, tags: ["ops"] } });
    expect(screen.getByLabelText("Filter by tag")).toBeTruthy();
    expect(screen.getByLabelText("Stop filtering by tag ops")).toBeTruthy();
  });

  it("counts a tag selection as an active filter, so Clear filters appears", () => {
    // Without this, the only way back to the full board was a page reload.
    renderTags({
      tasks: [],
      filters: { ...FILTERS, tags: ["ops"] },
      onClearFilters: () => {},
    });
    expect(screen.getByTestId("tasks-clear-filters")).toBeTruthy();
  });

  it("renders no chip row for an untagged task", () => {
    renderTags({ tasks: [{ ...tagged, tags: undefined }] });
    expect(screen.queryByLabelText(/^Filter by tag /)).toBeNull();
  });
});
