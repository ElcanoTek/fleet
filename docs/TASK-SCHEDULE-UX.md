# Create Task schedule controls

The Operations Center's **Create New Task** modal offers three schedule modes:
run immediately, run once at a local date and time, or repeat.

Repeat defaults to a plain-language builder for daily, weekday, and weekly
schedules. It translates the selected frequency, weekday, and local time into
the existing five-field cron value before submission, so this UI change does
not add a new API or persistence format. **Advanced cron** exposes the raw field
for schedules the builder cannot express; the existing presets and human-readable
next-run preview remain available. Repeat always shows its computed next run
prominently and updates it as the schedule changes; the preview is derived from
the recurrence, so it cannot contradict the saved schedule.

The run-once date and time controls use a bounded responsive grid. They render
side by side when space permits and stack on narrow mobile viewports, avoiding
the browser-specific intrinsic widths that previously overflowed the modal.

## Deliberately deferred

- The simple builder covers daily, weekdays, and one weekday per week. Monthly,
  interval, and multi-day schedules still use Advanced cron.
- Task records continue storing cron, not a second recurrence representation.
- A distinct first-run/start date for a recurrence remains deferred. The current
  scheduler treats recurrence as the source of truth for the next run; adding a
  separate editable date needs explicit skip/double-run and post-first-run
  semantics rather than overloading the existing one-shot field.
- Times use the operator's local timezone, matching the modal's existing
  behavior; this change does not add per-task timezone selection.
