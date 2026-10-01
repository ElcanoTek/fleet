# Recent Tasks search and the scheduled filter

The Operations Center's Recent Tasks board has one search box and, since this
change, one way to filter scheduled work. This note records what each does.

## Why it changed

Users reported jobs they could not find. The search was a single substring of
the whole input against `title`, `prompt` and the ID
(`title ILIKE '%<input>%' OR …`). Tested against Postgres, that missed a job
whenever it was remembered differently from how it was typed:

- words in another order: `sales weekly` did not find "Weekly sales report";
- a doubled space, or a stray one inside the input;
- a tag, the task's description, or the creator shown in the **Created By**
  column, none of which were searched.

It also treated a typed `%` or `_` as a SQL wildcard, so `100%` matched
anything containing `100`.

## What search does now

`db.taskSearchTerms` and `db.taskSearchTermClause` (`internal/sched/db/task_queries.go`):

- A full task ID still matches that task exactly.
- Otherwise the input is split on whitespace. A double-quoted run is one
  phrase (an unterminated quote runs to the end). **Every** term must match,
  in any order, and each may appear in any one of: title, prompt, description,
  `name` (the import/export identity), the ID text (so a short prefix works),
  any tag, or the creator's username.
- `%`, `_` and `\` match literally (`ILIKE … ESCAPE '\'`).
- At most 8 terms are used. Each term scans several text columns per row, so
  a pasted paragraph is capped rather than multiplying the query's cost; words
  past the eighth are ignored.
- Search ANDs with every other filter. It covers exactly the rows the caller
  may see: the own-rows visibility filter (#1082) still applies, so matching on
  a creator's name only narrows what the board could already show.

`TestTaskSearchFindsJobsTheWayPeopleRememberThem` pins each case above against a
real database (it skips without `DATABASE_URL`).

## One scheduled filter

The board had a **Scheduled Only** checkbox and a **scheduled** entry in the
**Status** dropdown. They meant different things: the checkbox kept every row
with a `scheduled_for` or a recurrence, which includes every past occurrence of
a repeating job (each occurrence is its own row); the dropdown keeps jobs in
the `scheduled` status, waiting for their run. Two controls with overlapping
names read as a duplicate, and the checkbox mostly showed history. The
checkbox is gone; **Status → scheduled** is the one filter, and shows one-off
jobs set for later plus the next occurrence of each repeating job.

## Deliberately not changed

- The API's `scheduled_only=true` parameter on `GET /tasks` still works for API
  callers. Only the board's checkbox (and the web proxy's pass-through of the
  parameter) was removed.
- No index. Search was already a leading-wildcard scan; adding a `pg_trgm` GIN
  index would need `CREATE EXTENSION` on every deployment (the reasoning in
  migration 060). Matching more columns makes each row's check a little more
  expensive, bounded by the 8-term cap.
- No ranking. Results keep the board's newest-first order.
- API-key creators are not matched by name: tasks created by an API key have no
  username to search.
