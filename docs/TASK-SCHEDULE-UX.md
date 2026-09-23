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
  DST rules never shift another zone's preview; a time the zone skips (a
  spring-forward gap) is not an occurrence, and a fall-back hour's repeated
  time can match twice — both as robfig/cron's `Next` does. A throwaway
  differential run (not committed) compared the preview with robfig's `Next`
  over ~110k samples across 13 zones and a year, and with a minute-by-minute
  brute-force search over ~174k samples every 5 minutes within ±30h of every
  2026 transition in 11 zones: it matched the brute force everywhere, and
  matched robfig everywhere except Australia/Lord_Howe (30-minute DST) and
  Pacific/Chatham, where robfig itself skipped a day around the transition
  (e.g. Lord Howe `0 3 * * *` from 2026-04-04 03:00 returned Apr 6, not Apr 5).
  The scheduler now computes every zone-aware next run through
  `internal/cronnext.Next`, the same algorithm in Go, so the preview and the
  scheduler agree there too (pinned by `TestNext_LordHoweDoesNotSkipADay` and a
  brute-force differential test around every 2026 transition). Days with no offset
  change are computed by arithmetic, and the form memoizes the preview and
  the zone list, so neither re-runs on unrelated keystrokes. The zone label's
  abbreviation is the one in effect at that next run, and the "not your own
  time zone" hint compares canonical names, so an alias such as `US/Eastern`
  counts as `America/New_York`. It is still a date-only
  preview, not the scheduler: the saved `scheduled_for` is the server's.

Not changed: existing tasks are not migrated — one saved in UTC stays in UTC
until someone edits it, because the server cannot know which zone its author
meant. **Run once** still takes the browser's local date and time (it
stores an absolute instant, so it was never affected).

**End repeat → On a date** now means 23:59:59 of that day in the repeat's
zone (`endOfDayInZone`), and an edited task reads its end date back in that
zone (`dateInZone`) instead of off the UTC timestamp, which showed the next
day for every zone west of UTC. The next-run preview re-evaluates once a
minute while the form is open, so it never keeps showing a run that has
passed.

## Deliberately deferred

- The simple builder covers daily, weekdays, and one or more weekdays per week. Monthly,
  interval, and multi-day schedules still use Advanced cron.
- Task records continue storing cron, not a second recurrence representation.
- A distinct first-run/start date for a recurrence remains deferred. The current
  scheduler treats recurrence as the source of truth for the next run; adding a
  separate editable date needs explicit skip/double-run and post-first-run
  semantics rather than overloading the existing one-shot field.
