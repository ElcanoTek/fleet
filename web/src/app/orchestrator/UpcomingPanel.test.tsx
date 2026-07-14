import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, cleanup, fireEvent } from "@testing-library/react";
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

// ── week view ────────────────────────────────────────────────────────────────

// Local-time helper: a run N days from now at a fixed hour, so the day-bucket
// math is deterministic regardless of when the test runs.
function inDays(n: number, hour = 9): string {
  const d = new Date();
  const local = new Date(d.getFullYear(), d.getMonth(), d.getDate() + n, hour, 0, 0);
  return local.toISOString();
}

describe("UpcomingPanel week view", () => {
  afterEach(() => window.localStorage.clear());

  it("shows a fixed Sun…Sat board with today highlighted and runs on their weekday", async () => {
    mockRuns([
      { task_id: "t1", prompt: "Morning sweep", next_run: inDays(0, 23), recurring: true, recurrence: "0 9 * * *" },
      { task_id: "t3", prompt: "Way later", next_run: inDays(30), recurring: false },
    ]);
    render(<UpcomingPanel />);
    await screen.findByTestId("upcoming-timeline");

    fireEvent.click(screen.getByTestId("upcoming-view-week"));
    const board = await screen.findByTestId("upcoming-week");
    const days = board.querySelectorAll(".upcoming-week-day");
    expect(days).toHaveLength(7);
    // fixed calendar shape: column i is always weekday i (Sun=0)
    const dow = new Date().getDay();
    expect(days[0]).toHaveTextContent(/Sun/i);
    expect(days[6]).toHaveTextContent(/Sat/i);
    // today's column is highlighted and carries today's run
    expect(days[dow].className).toContain("upcoming-week-day--today");
    expect(days[dow]).toHaveTextContent("Morning sweep");
    // days earlier this week recede but stay in place
    for (let i = 0; i < dow; i++) {
      expect(days[i].className).toContain("upcoming-week-day--past");
    }
    // outside the week: summarized, not silently dropped
    expect(board).toHaveTextContent(/1 more scheduled run\(s\) after this week/);
    expect(screen.queryByText("Way later")).toBeNull();

    // back to the list
    fireEvent.click(screen.getByTestId("upcoming-view-list"));
    await screen.findByTestId("upcoming-timeline");
    expect(screen.queryByTestId("upcoming-week")).toBeNull();
  });

  it("marks empty days instead of collapsing them", async () => {
    mockRuns([
      { task_id: "t1", prompt: "Only run", next_run: inDays(0, 23), recurring: false },
    ]);
    render(<UpcomingPanel />);
    await screen.findByTestId("upcoming-timeline");
    fireEvent.click(screen.getByTestId("upcoming-view-week"));
    const board = await screen.findByTestId("upcoming-week");
    expect(board.querySelectorAll(".upcoming-week-day")).toHaveLength(7);
    expect(board.querySelectorAll(".upcoming-week-empty").length).toBe(6);
    expect(board.querySelectorAll("[data-testid=upcoming-week-run]")).toHaveLength(1);
  });

  it("remembers the chosen view across mounts", async () => {
    mockRuns([]);
    const first = render(<UpcomingPanel />);
    await screen.findByTestId("upcoming-view-week");
    fireEvent.click(screen.getByTestId("upcoming-view-week"));
    expect(window.localStorage.getItem("fleet-upcoming-view")).toBe("week");
    first.unmount();

    render(<UpcomingPanel />);
    const weekBtn = await screen.findByTestId("upcoming-view-week");
    expect(weekBtn.getAttribute("aria-checked")).toBe("true");
  });
});

describe("UpcomingPanel week navigation", () => {
  it("pages to next week and back; prev disabled on the current week", async () => {
    mockRuns([
      { task_id: "t1", prompt: "This week run", next_run: inDays(0, 23), recurring: false },
      { task_id: "t2", prompt: "Next week run", next_run: inDays(7, 9), recurring: true, recurrence: "0 9 * * *" },
    ]);
    render(<UpcomingPanel />);
    await screen.findByTestId("upcoming-timeline");
    fireEvent.click(screen.getByTestId("upcoming-view-week"));
    await screen.findByTestId("upcoming-week");

    expect(screen.getByTestId("week-label")).toHaveTextContent("This week");
    expect(screen.getByTestId("week-prev")).toBeDisabled();
    expect(screen.getByTestId("upcoming-week")).toHaveTextContent("This week run");
    expect(screen.getByTestId("upcoming-week")).not.toHaveTextContent("Next week run");

    fireEvent.click(screen.getByTestId("week-next"));
    expect(screen.getByTestId("week-label")).toHaveTextContent("Next week");
    expect(screen.getByTestId("week-prev")).not.toBeDisabled();
    expect(screen.getByTestId("upcoming-week")).toHaveTextContent("Next week run");
    expect(screen.getByTestId("upcoming-week")).not.toHaveTextContent("This week run");

    fireEvent.click(screen.getByTestId("week-prev"));
    expect(screen.getByTestId("week-label")).toHaveTextContent("This week");
  });
});
