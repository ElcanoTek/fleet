# Recurrence end conditions + horizon-based Upcoming projection

## What shipped

Two scheduler UX gaps closed in one change:

1. **Recurring tasks can now end.** A recurring task may declare when its
   repeat chain stops — on a date (`recurrence_until`, RFC3339) and/or after a
   total number of runs (`recurrence_remaining`, 1–10000). Both are optional;
   absent means repeat forever (byte-identical behavior for every existing
   task). The task modal exposes them as **End repeat: Never / On a date /
   After a number of runs**.
2. **The Upcoming view projects to a horizon, not a count.** `GET
   /tasks/upcoming` previously projected at most 5 future occurrences per
   recurring task, so a weekly task looked like it "ended" five weeks out in
   the calendar views. With the new `?until=RFC3339` parameter the projection
   is horizon-based: every occurrence inside the window is emitted (per-task
   safety cap 366, `?limit` cap 1000). Without `?until` the count-based
   behavior is unchanged. The web Upcoming panel now requests a 14-day
   horizon. Both modes honor a task's own end conditions.

## How the end conditions work

Each occurrence of a recurring task is a **new task row**, cloned from the
completing one via the `TaskToCreate` recipe (see #565). The end conditions
are definition fields on that recipe:

- `recurrence_until`: `scheduleNextRecurrence` refuses to spawn an occurrence
  whose fire time is past it. The completing occurrence keeps its own
  terminal status; the chain simply stops.
- `recurrence_remaining`: counts the runs still allowed **including the row
  it sits on**. Each spawned occurrence carries `remaining - 1`; when the
  completing occurrence's value is ≤ 1, no next occurrence is spawned. So a
  task created with `recurrence_remaining: 5` runs exactly 5 times.
- Skips (#269 `run_if` gates) advance the same row without decrementing the
  budget — a skipped tick is not a run. If a skip's next cron tick would land
  past `recurrence_until`, the row is cancelled instead of being advanced
  (leaving it due would re-skip forever).

Validation at the boundary: either field without a `recurrence` is rejected;
`recurrence_remaining` must be 1–10000. A `recurrence_until` already in the
past is allowed (the first occurrence still runs; the chain just doesn't
continue) so exported definitions stay importable.

## Deliberate scope

- Export/import (#238) and clone/re-run (#270) carry both fields; the
  replace-by-name import path overlays them like every other definition
  field.
- The web Upcoming panel uses a fixed 14-day horizon rather than wiring the
  week board's navigation to the query — the board currently shows only the
  current week, so a navigable-range fetch is deferred until the board grows
  navigation.
- No "ends" badge in the tasks table yet; the modal round-trips the fields on
  edit.
