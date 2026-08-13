# Task titles

A task carries an optional **title**: a short operator-facing label shown
wherever the task is listed. It is the answer to "which job is this?" in the
Operations Center.

## Why it exists

The Recent Tasks table identified a task by the first ~80 characters of its
prompt. When several jobs open with similar boilerplate — the same persona
preamble, the same "You are producing a client-ready report…" — the list becomes
unreadable, so operators started writing a title line at the head of the prompt
to tell their jobs apart. That works, barely, but it puts display text into the
model's input, and it means the label and the instruction can never be edited
independently.

## Why it is not the `name` column

fleet already had a `tasks.name` column (migration 036). It cannot serve as a
display label, for two independent reasons:

1. **It is an identity, not a label.** `name` is the key the task-definition
   import/export endpoints (`GET /tasks/export`, `POST /tasks/import`) use for
   conflict detection, and it carries a *partial unique index* on non-empty
   values. Two tasks may never share one. But "Daily deal health scan" is
   exactly the kind of label that legitimately repeats across jobs.
2. **It does not survive a recurrence.** `storage.scheduleNextRecurrence`
   deliberately clears `Name` on every occurrence it spawns, because carrying it
   would collide with the row it was cloned from. A name therefore survives
   exactly *one* run of a recurring task — the opposite of what a display label
   needs.

`title` has neither constraint. It is non-unique, and it is carried by
`models.TaskToCreate`, which is the single canonical Task→TaskCreate clone used
by both the recurrence chain and re-run/clone. So every occurrence, every
re-run, and every clone of a job lists under the same title.

| | `name` | `title` |
|---|---|---|
| Purpose | import/export identity key | operator-facing display label |
| Unique | yes (partial index on non-empty) | no |
| Survives a recurrence occurrence | no — cleared per spawn | yes |
| Survives re-run / clone | no — cleared per copy | yes |
| Set from | `POST /tasks/import`, API | the create/edit form, API |
| Shown in the UI | nowhere | everywhere the task is listed |

## Contract

- **Optional.** Empty = untitled, which is every task that existed before this
  shipped. Untitled tasks render exactly as they did: the prompt's first line.
- **Single line, ≤120 runes.** Validated server-side (`maxTaskTitleChars`) and
  mirrored in the form. It is rendered inline in a table cell and a calendar
  tile, so an embedded newline is rejected rather than silently mangled.
- **Trimmed** on the way in.
- **Never injected into the agent's prompt.** Like `description`, it is operator
  documentation. Titling a task changes nothing about what the model sees.
- **Editable.** `PUT /tasks/{id}` accepts it (via `storage.TaskEdit`), and
  `POST /tasks/{id}/rerun|clone` accepts a `title` override so a one-off copy
  can be relabelled without touching the source.

## Where it shows up

- **Recent Tasks** — the `Task` column leads with the title and demotes the
  prompt to a muted second line; the phone card does the same. Untitled tasks
  are unchanged.
- **Search** — the task filter matches `title` alongside `prompt` and `id`, so
  the thing you see in the list is the thing you can search for.
- **Upcoming Runs** — the timeline and week board label a projected run with its
  title. (They previously preferred `name`, which only ever appeared on a
  recurring task's *first* occurrence.)
- **The task-detail modal** — the header names the task by its title.
- **The SLA report** — rows group by title when present, so a titled job's
  occurrences collapse into one row instead of one row per prompt variant.
  `slamonitor.TaskName` (the metric label) prefers it too, which also bounds
  that label's cardinality more tightly than a free-form prompt could.
- **Templates** — a bundle template may set `task.title`; the create form
  otherwise seeds the title from the template's own name, which is what the
  operator picked it by. Either way it stays editable.

## Storage

Migration `060_add_task_title` adds `title TEXT NOT NULL DEFAULT ''`, and no
index: the search matches with a leading-wildcard `title ILIKE '%q%'`, which no
btree can serve, and the trigram alternative would require a `CREATE EXTENSION`
of every deployment. The same query already scans `prompt` the same way. The
column threads the standard route for a new per-task field: `taskColumns` / `scanTask` / `AddTask` /
`taskInsertColumns` + `taskInsertArgs` + `taskInsertColumnsCount` /
`taskInsertOnConflict` / `UpdateTaskTx`, plus `TaskExportRecord` so a definition
keeps its title when it moves between deployments.

Each of those write paths has its own hand-maintained column list, so each has
its own round-trip test (`internal/sched/db/title_test.go`) — the multi-row
`AddTaskBatch` path especially, since that is the one a forgotten
`taskInsertColumnsCount` bump silently broke once before (#710).
