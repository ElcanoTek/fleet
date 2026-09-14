import type { StatFilter } from "./StatsGrid";

/** The filter fields a stats card sets on the board. */
export type StatFilterSelection = {
  status: string;
  completedToday: boolean;
  completedStatus: string;
};

// What each dashboard counter narrows the board to when you click it.
//
// Extracted from the click handler so it can be tested, because the defect this
// closes was a DISAGREEMENT between a counter and its own filter: Failed Today
// counted `error` only while most failures come to rest in `dead_lettered`, and
// the filter repeated the same omission — so a board that did show failures
// reported none, and clicking the number would have landed on an empty table.
//
// The rule to keep: every entry here must select exactly the rows its counter
// counts in GetDashboardStats. The two are in different languages (SQL and a
// query string) and cannot share code, so the test beside this file is what
// holds them together.
export function statFilterSelection(filter: StatFilter): StatFilterSelection {
  switch (filter) {
    case "tasks-pending":
      return { status: "pending", completedToday: false, completedStatus: "" };
    case "tasks-running":
      return { status: "running", completedToday: false, completedStatus: "" };
    case "tasks-completed-today":
      return { status: "", completedToday: true, completedStatus: "success" };
    case "tasks-failed-today":
      // Both terminal failure statuses: a failure comes to rest in
      // dead_lettered (a deterministic one on its first attempt, a retryable
      // one once its retry budget is spent) or, less often, in error.
      return { status: "", completedToday: true, completedStatus: "error,dead_lettered" };
  }
}
