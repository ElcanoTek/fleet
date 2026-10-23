# The daily owner digest

Once a day, each task owner gets one email listing their scheduled tasks
that are not working, so a stale client dashboard is found by its owner the
next morning instead of by hand days later. It is **off by default**; an
admin turns it on under Settings → Admin → **Notifications**.

Code: `internal/ownerdigest` (the job), `db.OwnerDigestCandidates`
(`internal/sched/db/owner_digest.go`, the query), `store.ClaimOwnerDigestDay`
(`internal/store/owner_digest.go`, the once-a-day claim),
`notify.Notifier.SendDirectEmail` (the send). Migration 083 (chat store).

## What an owner receives

One email, subject `Fleet daily digest: <n> scheduled tasks need attention`,
with up to four sections, most severe first. Each line names the task (its
title, or its prompt on one line), when, the reason in the run's own recorded
words, and a link to the task's record (`<FLEET_PUBLIC_URL>/orchestrator/tasks/<id>`,
built exactly as a notification's log link; the task ID instead when no
public URL is set).

| Section | A task is listed when | Reason shown |
| --- | --- | --- |
| **Schedule stopped** | its latest run in the last 24 h dead-lettered and parked its recurring schedule (`recurrence_parked_at`, ADR-0070) | the park reason (`recurrence_parked_reason`), then the run's failure |
| **Failed** | its latest run in the last 24 h dead-lettered or errored | `dead_letter_reason`, else `error_message` |
| **Blocked** | its latest run in the last 24 h succeeded with run outcome `blocked` ([ADR-0092](adr/0092-blocked-is-a-notification-status.md)) | `run_outcome_detail` |
| **Not started on time** | an occurrence is still `scheduled` or `pending` more than 2 h after its `scheduled_for` | how late it is; a note when the task has a `run_if` check, which may be what holds it |

Rules that decide what counts:

- **Only scheduled work.** A recurring occurrence, or a one-off with a
  `scheduled_for`. A task run on the spot is watched by whoever started it.
  Webhook templates never run, so they are never overdue.
- **The latest run per job.** "Job" is `lineage_id`
  ([ADR-0069](adr/0069-per-job-scheduled-workspaces.md): shared by every
  occurrence, re-run and clone), per owner. A later success, or a cancelled
  run, of the same job drops an earlier problem: the digest says what is
  still wrong, not everything that went wrong today. A clone by a different
  owner cannot hide this owner's problem, because the owner is part of the
  key.
- **The last 24 hours** for finished runs (by `completed_at`, which a
  dead-letter also stamps). An overdue occurrence is listed for as long as it
  stays overdue.
- **Nothing to report, no email.**
- **Owner address.** `created_by` resolves to the account email exactly as the
  runner resolves a notification's push audience (`GetUsersByIDsWithContext`;
  the sched username is the email on a standard deployment). An owner whose
  username is not a plain email address (a local `admin`) is skipped and
  counted in the log line. Ownerless tasks are skipped.
- At most 50 lines per email; the rest are counted ("…and 7 more").

## Settings

Two fields on the admin notification settings row (`notify_settings`,
migration 083), saved with the rest of the panel and hot-swapped into the
live notifier like every other notification setting:

| Field (API) | Default | Meaning |
| --- | --- | --- |
| `owner_digest_enabled` | `false` | send the digest |
| `owner_digest_hour_utc` | `13` | the UTC hour (0–23) at or after which the day's digest goes out |

To enable it:

- **UI:** Settings → Admin → Notifications → **Daily owner digest**: turn the
  switch on, pick **Send at (UTC)**, and **Save notification settings**. The
  card shows **Needs SMTP** while the saved settings lack an SMTP host or a
  From address.
- **API:** `PUT /api/admin/notify-settings` (admin only) with the whole
  settings object, including `"owner_digest_enabled": true` and optionally
  `"owner_digest_hour_utc": 9`. The PUT replaces the row, so send the current
  values of the other fields too (`GET` first). An hour outside 0–23 is a 400.

There is no env var: under the env-derived config (no saved admin row, or
after **Use env config**) the digest is off.

The digest needs **an SMTP host and a From address** — the same rule as email
reply-back — but not the recipients list: each email goes to the task owner,
never to the deployment-wide `EmailTo` list, and the *Notify on* filter does
not apply to it. Without SMTP the day's digest is a logged no-op
(`owner digest: not sent for <day> — SMTP is not configured …`, once a day).

## When it runs, and once a day

A loop started with `fleet serve` checks every minute. The day's digest is
due on the first check at or after the configured hour (UTC). So a process
that was down at that hour sends on its first check after it comes back, and
an admin who turns the digest on after the hour gets that day's digest within
a minute.

Exactly-once per day per deployment is a persisted claim, not a timer:

1. Read the candidates and build the digests (a read failure retries next
   minute; nothing is claimed).
2. Claim the UTC day: `INSERT INTO owner_digest_days … ON CONFLICT DO
   NOTHING`. Only the claim that inserted the row sends. A restart, a second
   replica, or a later minute finds the day claimed.
3. Send, one email per owner, each with notify's per-attempt timeout and
   bounded retry, within a 15-minute budget for the whole day.

The claim comes before the sends, so the guarantee is **at most once**: a
crash, or an SMTP outage, after the claim loses (part of) that day's digest
rather than sending anyone a duplicate. A failed delivery is logged with the
recipient and is not retried that day. Claims older than 90 days are pruned.
A digest with nobody to tell still claims the day.

Changing the hour after the day's digest went out does not send a second one.

## Deviations and what was left out

- **"Latest run" is per job, not per row chain.** The brief said "tasks whose
  latest run was blocked"; the job identity used is `lineage_id`, which also
  joins re-runs and clones of the job by the same owner, so a successful
  re-run of a dead-lettered occurrence clears it from the digest.
- **A stopped schedule is listed for the day it stopped**, like every other
  finished-run problem, not every day until someone acts. The Operations
  Center keeps showing **⏹ Schedule stopped**.
- **Disabled accounts are not filtered.** The owner lookup is the same as the
  runner's, which does not check the account's enabled flag.
- **No preview or test send**, and no per-owner opt-out or per-team routing.
  A digest goes to every owner with a problem, while the deployment-wide
  switch is on.
- **The window and the overdue threshold are fixed** (24 h, 2 h), and the
  hour is UTC only.

## Tests

- `internal/ownerdigest/ownerdigest_test.go`: classification of every kind and
  every exclusion (healthy, outside the window, unscheduled, ownerless,
  cancelled, not yet overdue, webhook template, running), grouping per owner
  with severity order and dedupe, the per-owner cap, rendering and escaping,
  and the tick: once per day across ticks, a restart and the next day; off
  and before-the-hour; the SMTP-not-configured logged no-op (and sending once
  SMTP is configured the same day); read and claim failures retried; one
  owner's failed send not stopping the others.
- `internal/sched/db/owner_digest_test.go`: the candidate query against
  Postgres (latest-per-job, clones by another owner, the window, overdue
  cron occurrences, the limit).
- `internal/store`: `TestClaimOwnerDigestDayOncePerDay` (concurrent claimers,
  a second claim the same UTC day, the next day, pruning) and
  `TestNotifySettingsOwnerDigestFields`.
- `internal/notify/direct_test.go`: `SendDirectEmail` against an in-process
  SMTP server addresses only the owner, and refuses without host/From.
- `internal/notifyadmin`, `internal/httpapi`: the settings round-trip,
  hot-swap and Revert, and the 400 on a bad hour.
- Web: `notifications.test.tsx` (the switch, the hour, the Needs SMTP badge)
  and the mocked Playwright admin spec.
