# The Operations Center

Where your recurring work lives. This guide covers what you will see on the
screen, what a task is made of, what every run state means, and what to do when a
run needs your attention.

## 1. What the Operations Center is

The Operations Center is the home for work that runs on its own: the reports,
checks, and analyses your team has decided are worth repeating. This is where
that work gets set up and checked on; its results arrive in your inbox.

Everything in it follows one loop. A conversation with the assistant produces
something worth keeping. Saving it as a workflow turns that conversation into a
prompt in the prompt library. The prompt becomes a scheduled task. The task runs
on its own and emails the result to whoever should see it.

> conversation → saved prompt → scheduled task → your inbox

Everything else in this guide is detail on that loop: how to read the board, how
to shape a task, and how to respond when a run asks something of you.

## 2. Reading the screen

### The top of the board

Counters summarize the moment: **Agent Slots** (how many runs can execute at
once), **Active Agents**, **Pending Tasks**, **Running Tasks**, **Completed
Today**, and **Failed Today**. A healthy quiet day reads mostly zeros with a
green completed count. Each of the four task counters is also a filter — click
one to narrow the board to exactly those rows.

The clock in the corner shows **Server Time** in UTC. Every timestamp on the
board is anchored to it, so a task scheduled for the afternoon in your local time
will show a UTC time here. When in doubt about when something ran, trust the
server clock.

### The tabs

| Tab | What it shows |
| --- | --- |
| **Recent Tasks** | The ledger. Every task, newest first, with its status and history. This is where you will spend most of your time, and the rest of this guide assumes it. |
| **Upcoming** | A forecast, not a log: the runs the scheduler is about to perform, grouped by day (Today, Tomorrow, then dates). It is computed from the current schedules, so editing a schedule updates it immediately. |
| **Datasets** | Background row-by-row work over tables, when your deployment uses it. |
| **SLA**, **Usage**, **Adoption** | Tasks measured against an expected duration, and spend and activity reporting. Admin-only: these tabs do not appear for other roles. |

None of the tabs beyond the first two are required for daily work.

### Anatomy of a task row

Each row in **Recent Tasks** shows the task's short **ID**, its **title** in bold
with the opening of its prompt in muted text beneath, a **status badge**, its
**schedule**, **who created it**, and **when**. Clicking the row opens the task's
full record.

On the right of every row sit four actions:

| Action | What it does |
| --- | --- |
| **Run now** | Queue an immediate run, on top of whatever schedule exists. |
| **Edit task** | Open the task for changes. See [Everyday actions](#5-everyday-actions) for what editing means on a finished task. |
| **Stop this task** | The task will not run again. Its history stays on the board. A deliberate stop is not treated as a failure: nothing retries, nothing alerts. |
| **Delete** | Removes the task **and its run history** permanently, and frees its name for reuse. Prefer Stop when you simply want something to cease running; delete only what should leave the record. |

A task that is executing right now is the exception: while a run is live it can
only be stopped. Run-now, edit, and delete are all withheld until it finishes, so
a second copy can never race the first and a running record can never be erased
underneath itself.

### Finding things

The board filters by **Status** and **Created by**, narrows to **Scheduled
only**, and searches across title, prompt, and ID.

### Whose tasks you see

The board shows the tasks **you created**. Workspace admins see everyone's, which
is also where the **Created by** filter earns its keep. The same rule follows the
tasks everywhere: the **Upcoming** projection, records, and logs cover your own
work unless your role is wider. Note that this governs who sees the task on the
board, not who receives its results; a task's emails go to its recipients
regardless.

> **Why the board fills up.** Every occurrence of a recurring task is its own
> row. A daily report produces a new entry each day rather than updating
> yesterday's, so the table is a true ledger of what ran and when. Filters and
> search are how you work with it, not a workaround.

## 3. What a task is made of

A task is a handful of decisions: what to call it, what it should do, when it
should run, who hears about it, what it may touch, and the context that travels
with it.

| Field | What it decides |
| --- | --- |
| **Title** | The label the whole board shows. It is for people only and never enters the assistant's instructions, so name tasks the way your team talks. When you insert a prompt from the library, the task takes the prompt's name as its starting title, and a task created in conversation takes the name you confirm on its approval card. |
| **Prompt** | The instructions, pulled from the **prompt library** or pasted in. The library entry is the single source of truth: when a report needs fixing, fix the library prompt, then update the task with the corrected version. Library edits do not reach existing tasks on their own. Avoid hand-editing instructions inside a task. |
| **Schedule** | Three modes: **Run now**, **Run once** at a date and time, or **Repeat**. Repeat offers a plain-language builder for daily, weekday, and weekly patterns, and an advanced field for anything else. A repeat can also end on its own, under **End repeat**: never, on a date, or after a set number of runs. The form always previews the computed next run; read it before launching. |
| **Recipients** | The email addresses that receive each run's result. Recipients belong to the task, not the prompt, so the same library prompt can serve different audiences. |
| **Tools & files** | What the task may reach: mailboxes, connectors, files. Some connections are always on for every run; the rest are selected per task, so new tasks start with your deployment's recommended set and an existing task never silently gains new connections. Files can also be attached directly to the task, for work that runs against a fixed reference like a template or a lookup table. |
| **Context** | Notes that travel with the task for the people who operate it: why it exists, who owns it, what to do if it fails. These are shown to operators and never enter the assistant's instructions. Alongside them sit **tags** for filtering the board and the task's **persona**, which is left blank for the workspace default unless the task genuinely needs a different one. |
| **Advanced** | Further settings, including the model the task runs on and an option for a recurring task to carry a short summary of its previous run into the next one. The model in particular is worth choosing deliberately: match it to the demands of the job rather than leaving it to chance. |

As you fill the form in, a **cost forecast** appears beneath it: the token
breakdown for the run you are describing and, where the model's pricing is known,
a dollar estimate with a range. It warns when the estimate would exceed the
deployment's cost ceiling. It is advisory — it never blocks the submit — but it
is the cheapest moment to notice that a daily job is about to be an expensive
one.

## 4. Run states

Every status badge means one of ten things. What each one means, and what to do:

| Status | What it means, and what to do |
| --- | --- |
| `SCHEDULED` | Waiting for its time. Nothing to do. |
| `PENDING` | Due and queued for a free agent slot. Nothing to do; it will start on its own. |
| `LEASED` | An agent has claimed it and is starting it now. A brief, normal step between pending and running. |
| `RUNNING` | Executing now. Open it to watch live if you are curious. |
| `SUCCESS` | Finished and delivered. The result is in the recipients' inboxes and in the logs. |
| `ERROR` | The run failed. If the cause was transient — a hiccup in a connection or a service — it retries by itself with increasing delays. If the cause was deterministic, something that would fail identically every time, it fails once, on purpose, and waits for a person. |
| `DEAD_LETTERED` | Failed and out of retries, set aside for review. The row keeps its full record. Read the failure, fix the cause, and rerun; see [the next section but one](#6-when-a-run-goes-wrong). |
| `CANCELLED` | A person stopped it. The record notes who. Deliberate stops never retry or alert. |
| `PAUSED_AWAITING_INPUT` | The run reached a decision it was not willing to make alone and stopped to ask a question. It holds no resources while it waits. Answer it and it resumes from there; this state is the system working as designed, not a failure. |
| `PAUSED_AWAITING_WAKE` | The run put itself to sleep until a set time or an expected event, for example to check back on something later. It wakes on its own; every sleep has a deadline. Sleeping runs also get their own short list on the dashboard, so parked work stays visible without a status filter. |

> **The retry rule in one line.** Transient failures retry themselves;
> deterministic failures wait for you. A task that failed and did not retry is
> telling you a rerun would fail the same way until something changes.

## 5. Everyday actions

### Run a task on demand

**Run now** on any row queues an immediate run. Use it after fixing a prompt, or
when someone needs today's report ahead of schedule. It does not disturb the
task's regular schedule.

### Open a task's record

Click any row to open the task's record: the full transcript of what the run did,
its tool calls and results, token and cost figures, and the final output. The
record carries its own action strip, so **Edit**, **Resubmit**, **Delete**,
**History**, **Discuss in chat**, and **Download logs** are all reachable from
here without returning to the board. For a **running** task the same view
attaches live, streaming each step as it happens. When the run maintains a plan,
the live view shows it as a checklist ticking from to-do to done. A **Stop run**
button is there if a live run needs halting; the record will note who stopped it.

### Edit a task

Editing a task that has not started yet simply changes it. Editing a task that
has already finished works differently, and the form says so: saving
**resubmits a new copy** with your changes rather than rewriting history. The
original row stays exactly as it ran. This is deliberate: the ledger records what
actually happened, and changes create new entries.

### Answer a paused task

When a task pauses to ask a question, the question is waiting in its record, and
a notification goes out. Provide the answer there and the task re-queues and
continues with your answer in hand. There is no rush: a paused task consumes
nothing while it waits.

### Rate a run

Each finished run can be marked **Helpful** or **Not helpful**, with an optional
critique. This is the lightest way to steer recurring work: patterns in the
feedback surface as suggested adjustments that an operator reviews before
anything changes.

## 6. When a run goes wrong

Work down this list. Most problems resolve at the first or second step.

**1 · Read the report.** Well-built reports flag what they could not do rather
than guessing around it. A missing source or a skipped section is usually named
plainly in the output itself, along with what the run did instead.

**2 · Read the analysis.** A task that failed terminally carries an automatic
diagnosis: a classification of what went wrong and a suggested fix, attached to
the failed row.

**3 · Read the transcript.** The log shows every step the run took. If the task
retried, the record keeps each attempt; a picker in the log view switches between
the latest transcript and superseded ones, so an earlier failure is never papered
over by a later retry.

**4 · Ask about it.** **Discuss in chat** opens a conversation seeded with the
run's record, so you can ask questions in plain language: why a figure moved, why
a section is empty, what a step did. Interrogating the run beats re-reading it.

**5 · Escalate.** If the cause is outside the task — a feed that stopped
arriving, a connection that needs attention — bring it to your administrator
with the task title, the run's date, and the run's downloaded logs
(**Download logs** in the task's record). The logs are the most helpful thing you
can attach: they carry everything needed to trace it.

## 7. Working conventions

Four habits keep a shared board legible as it grows.

| Habit | Why |
| --- | --- |
| **Title everything** | A good title says what the job is in your team's words. A task without one falls back to showing its prompt's first line, which is rarely what you want scanning a board. |
| **One team version** | Each recurring report should exist as one scheduled task with a recipient list, not one task per person. Duplicates mean duplicate emails every morning and split run histories. And since a task's row lives on its creator's board, the team version belongs to whoever will maintain it: teammates receive its results either way, but they cannot see or rerun a row they do not own. |
| **Fix in the library** | When a report needs a change, change the library prompt, update the task to the corrected version, then **Run now** to confirm. The task is where a prompt runs; the library is where it lives. |
| **Keep the ledger** | Stop tasks that should no longer run; reserve deletion for things that should leave the record entirely. Six months from now, the history of what ran and how it went is the most useful thing on this screen. |
