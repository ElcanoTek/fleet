import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, cleanup } from "@testing-library/react";
import { UpcomingPanel } from "./UpcomingPanel";
import type { UpcomingRun } from "@/app/shared/lib/orchestratorApi";

// UpcomingPanel renders the GET /tasks/upcoming projection (Scheduler UX 2.0,
// #504): a day-grouped timeline of the next scheduled runs.

const upcomingRuns = vi.fn();
vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: {
    upcomingRuns: (...args: unknown[]) => upcomingRuns(...args),
  },
}));

afterEach(() => cleanup());

function mockRuns(runs: UpcomingRun[]) {
  upcomingRuns.mockReset();
  upcomingRuns.mockResolvedValue({ upcoming: runs });
}

// A far-future fixed timestamp so the "Today/Tomorrow" heuristic can't flake on
// the day the test runs — it will always render the weekday/date label.
const FUTURE = "2999-01-15T14:30:00Z";

describe("UpcomingPanel (#504)", () => {
  it("renders the empty state when nothing is scheduled", async () => {
    mockRuns([]);
    render(<UpcomingPanel />);
    await waitFor(() => {
      expect(screen.getByText(/No upcoming runs/)).toBeTruthy();
    });
  });

  it("renders a recurring run with its cron description", async () => {
    mockRuns([
      {
        task_id: "t1",
        name: "daily-report",
        prompt: "summarize yesterday",
        recurrence: "0 9 * * *",
        next_run: FUTURE,
        recurring: true,
      },
    ]);
    render(<UpcomingPanel />);
    await screen.findByTestId("upcoming-run-row");
    expect(screen.getByText("daily-report")).toBeTruthy();
    // The kind chip carries the recurring class + a human cron description.
    const chip = screen.getByText((_, el) =>
      (el?.className ?? "").includes("upcoming-run-kind-recurring"),
    );
    expect(chip).toBeTruthy();
  });

  it("labels a one-shot run as One-time and falls back to the prompt when unnamed", async () => {
    mockRuns([
      {
        task_id: "t2",
        prompt: "run the migration once",
        next_run: FUTURE,
        recurring: false,
      },
    ]);
    render(<UpcomingPanel />);
    await screen.findByTestId("upcoming-run-row");
    expect(screen.getByText("One-time")).toBeTruthy();
    expect(screen.getByText(/run the migration once/)).toBeTruthy();
  });

  it("groups runs on the same day under one header", async () => {
    mockRuns([
      { task_id: "a", name: "first", prompt: "p", next_run: "2999-01-15T09:00:00Z", recurring: true, recurrence: "0 9 * * *" },
      { task_id: "b", name: "second", prompt: "p", next_run: "2999-01-15T17:00:00Z", recurring: true, recurrence: "0 17 * * *" },
    ]);
    render(<UpcomingPanel />);
    await waitFor(() => {
      expect(screen.getAllByTestId("upcoming-run-row").length).toBe(2);
    });
    // Two runs, one shared day-group header.
    expect(screen.getAllByText(/Next 2 scheduled run/).length).toBe(1);
  });
});
