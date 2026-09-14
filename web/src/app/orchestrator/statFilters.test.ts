import { describe, expect, it } from "vitest";
import { statFilterSelection } from "./statFilters";
import type { StatFilter } from "./StatsGrid";

// A counter and the filter behind it must agree. They did not: Failed Today
// counted `error` only — while most failures come to rest in `dead_lettered` —
// and the filter repeated the omission, so a day of quarantined work reported
// zero and clicking the number would have shown an empty board.
describe("statFilterSelection", () => {
  it("asks for both failure states behind Failed Today", () => {
    const selection = statFilterSelection("tasks-failed-today");
    expect(selection.completedToday).toBe(true);
    const statuses = selection.completedStatus.split(",");
    expect(statuses).toContain("error");
    expect(statuses).toContain("dead_lettered");
  });

  it("keeps Completed Today to successes", () => {
    expect(statFilterSelection("tasks-completed-today")).toEqual({
      status: "",
      completedToday: true,
      completedStatus: "success",
    });
  });

  it.each(["tasks-pending", "tasks-running"] as const)(
    "%s filters by live status, not by a completion window",
    (filter) => {
      const selection = statFilterSelection(filter);
      expect(selection.completedToday).toBe(false);
      expect(selection.completedStatus).toBe("");
      expect(selection.status).toBe(filter.replace("tasks-", ""));
    },
  );

  // Every card must resolve to something. A card whose filter fell through
  // would clear the board rather than narrow it, which reads as "no such rows".
  it.each([
    "tasks-pending",
    "tasks-running",
    "tasks-completed-today",
    "tasks-failed-today",
  ] as StatFilter[])("%s has a selection", (filter) => {
    expect(statFilterSelection(filter)).toBeDefined();
  });
});
