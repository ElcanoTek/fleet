# Create Task schedule controls

The Operations Center's **Create New Task** modal offers three schedule modes:
run immediately, run once at a local date and time, or repeat.

Repeat defaults to a plain-language builder for daily, weekday, and weekly
schedules. Weekly schedules accept one or more weekday chips (for example,
Monday and Wednesday). It translates the selected frequency, weekdays, and local time into
the existing five-field cron value before submission, so this UI change does
not add a new API or persistence format. **Advanced cron** exposes the raw field
for schedules the builder cannot express; the existing presets and human-readable
next-run preview remain available. Supported cron expressions round-trip into
the friendly editor, so `0 9 * * 1,4` displays Monday and Thursday selected;
more complex expressions stay in Advanced cron rather than being misrepresented.
Repeat always shows its computed next run
prominently and updates it as the schedule changes; the preview is derived from
the recurrence, so it cannot contradict the saved schedule.

The run-once date and time controls use a bounded responsive grid. They render
side by side when space permits and stack on narrow mobile viewports, avoiding
the browser-specific intrinsic widths that previously overflowed the modal.

## Repeat time zone

A recurring task's cron fires at the wall-clock time in the task's own IANA
zone (`timezone` on the task; the server falls back to
`FLEET_DEFAULT_TIMEZONE`, then UTC, when a create request names none). The
modal used to send no zone while labelling the schedule "local time", so a
"weekdays at 8:00 AM" task created from New York fired at 08:00 UTC — 4 AM
Eastern. What shipped:

- Repeat has a **Time zone** picker. A new task defaults to the browser's zone
  (`Intl…resolvedOptions().timeZone`); an edited task opens on its stored zone;
  a template's `timezone` seeds it when set. The form always sends the zone
  with a recurrence, on create and on edit.
- The schedule echo names the zone ("At 08:00, Monday through Friday ·
  America/New_York (EDT)") instead of "local time", and a hint appears when the
  selected zone is not the viewer's — which is how an older task that was
  silently saved in UTC shows up. Changing the zone and saving moves the next
  run: the edit path re-derives `scheduled_for` from the cron in the request's
  zone (pinned by `TestUpdateTask_TimezoneChangeMovesNextRun`).
- The next-run preview evaluates in the selected zone
  (`nextCronOccurrence(expr, from, timeZone)`) and formats its date there. It
  scans the zone's calendar with DST-free UTC arithmetic, so the browser's own
  DST rules never shift another zone's preview, and a time the zone skips (a
  spring-forward gap) is not shown as an occurrence. It is still a date-only
  preview, not the scheduler: the saved `scheduled_for` is the server's.

Not changed: existing tasks are not migrated — one saved in UTC stays in UTC
until someone edits it, because the server cannot know which zone its author
meant. **End repeat → On a date** still means end of that day in the
browser's zone, and **Run once** still takes the browser's local date and time
(it stores an absolute instant, so it was never affected).

## Deliberately deferred

- The simple builder covers daily, weekdays, and one or more weekdays per week. Monthly,
  interval, and multi-day schedules still use Advanced cron.
- Task records continue storing cron, not a second recurrence representation.
- A distinct first-run/start date for a recurrence remains deferred. The current
  scheduler treats recurrence as the source of truth for the next run; adding a
  separate editable date needs explicit skip/double-run and post-first-run
  semantics rather than overloading the existing one-shot field.
