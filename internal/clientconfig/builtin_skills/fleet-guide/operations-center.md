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
Today**, and **Failed Today**. Each of the four task counters is also a filter —
click one to narrow the board.

One thing about those numbers is worth knowing before you trust them: they
count the **whole deployment**, while the board below shows only your own tasks
unless you are an admin. So a non-zero counter can filter down to fewer rows
than it promised, or to none — the difference is your colleagues' work, not a
bug.

**Failed Today** counts a run that ended in either failure state, `ERROR` or
`DEAD_LETTERED` (see [Run states](#4-run-states) for the difference) — both, not
just one of them, so it no longer hides the state most failures actually end in.

Read it as a count of what is **still** failed, though, not a tally of the day.
Replaying a dead-lettered task returns that row to `PENDING` and clears its
completion time, so it leaves the count; deleting the task removes it the same
way. Replay re-runs that occurrence. If the task is recurring and the
dead-letter already queued the next run, replay does not queue another; if the
schedule is parked (two consecutive dead-letters), replay is how it continues.
The one exception is a prompt whose **EXECUTION REQUIREMENTS (JSON)** line is
malformed. That schedule parks on its first dead-letter, and replay would rerun
the same broken line. Editing the dead-lettered task does not fix the schedule
either: saving an edit to a finished task starts a one-off run, not a new
schedule. To bring the schedule back, create the task again with the corrected
prompt and its schedule. Fleet refuses to save such a line in the first place
and names the bad entry, so this only affects tasks saved before that check.
A zero therefore means nothing is sitting failed from today — which is what
you usually want to know, but it is not the same as nothing having gone wrong.

The clock in the corner shows **Server time** — the wall clock in the zone the
deployment runs in, which is not necessarily your own and not necessarily UTC.
Hover it: the tooltip names that zone, and when new tasks default to a different
one for scheduling, it names that too. Those are the two zones that decide when
a recurring task fires; the timestamps in the task rows are something else
again, rendered in your browser's own zone. So when a schedule and a row seem to
disagree, they are usually both right and reading different clocks — the
tooltip is what settles it.

### The tabs

| Tab | What it shows |
| --- | --- |
| **Recent Tasks** | The ledger. Every task, newest first, with its status and history. This is where you will spend most of your time, and the rest of this guide assumes it. |
| **Upcoming** | A forecast, not a log: the runs the scheduler is about to perform, grouped by day (Today, Tomorrow, then dates). It is computed from the current schedules, so editing a schedule updates it immediately. |
| **Datasets** | Background row-by-row work over tables, when your deployment uses it. |
| **SLA**, **Usage**, **Adoption** | Tasks measured against an expected duration, and spend and activity reporting. Admin-only: these tabs do not appear for other roles. |

None of the tabs beyond the first two are required for daily work.

Administrators assign access under **Settings → Admin → Users**. Ops Center
offers **None**, **Viewer** (read-only), and **Contributor** (can create and run
tasks). **Fleet Admin** grants full Chat and Ops permissions; selecting it again
turns it off and defaults to Chat Contributor plus Ops None. Central Auth account
grants apply these selected roles to Fleet automatically.

### Anatomy of a task row

Each row in **Recent Tasks** shows the task's short **ID**, its **title** in bold
with the opening of its prompt in muted text beneath, a **status badge**, its
**schedule**, **who created it**, and **when**. Clicking the row opens the task's
full record.

On the right of every row sit four actions:

| Action | What it does |
| --- | --- |
| **Run now** | Queue an immediate run, on top of whatever schedule exists. Offered on a task that is waiting or finished — not on one that is in flight or paused (see below). |
| **Edit task** | Open the task for changes. See [Everyday actions](#5-everyday-actions) for what editing means on a finished task. |
| **Stop this task** | The task will not run again. Its history stays on the board. A deliberate stop is not treated as a failure: nothing retries, nothing alerts. |
| **Delete** | Removes the task **and its run history** permanently. Prefer Stop when you simply want something to cease running; delete only what should leave the record — you never have to delete a task to reuse its title, since titles are labels and need not be unique. |

Two kinds of row show fewer than four. While a run is **live** it can only be
stopped: run-now, edit and delete are withheld until it finishes, so a second
copy can never race the first and a running record can never be erased
underneath itself. A **paused** run — waiting on your answer, or asleep until its
wake time — also has no **Run now**, deliberately: what it needs is the answer or
the wake, not a second copy started alongside it. Stop is offered on both.

### Finding things

The board filters by **Status** and **Created by**, narrows to **Scheduled
only**, and searches across title, prompt, and ID.

It also filters by **tag**. A task's tags appear as small coloured chips on its
row; clicking one narrows the board to that tag, and the **Tags** dropdown in
the filter bar offers every tag on the tasks you can see (your own tasks by
default; every task if you are an admin or have the view-all grant), including
tags on tasks not on the current page. A tag just added to a task that is not on
the current page can take up to five minutes to appear in the dropdown by
default; tags on the tasks in front of you appear at once. Tags stack: each one
you add narrows the board further, to tasks carrying *all* of them. Remove a tag
by clicking its chip in the filter bar, or drop everything at once with **Clear
filters**.

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
| **Schedule** | Three modes: **Run now**, **Run once** at a date and time, or **Repeat**. Repeat offers a plain-language builder for daily, weekday, and weekly patterns, and an advanced field for anything else. A repeat fires at its time in the **Time zone** picked under it — your browser's own zone for a new task — and the preview names that zone, so "8:00 AM" means 8:00 AM there. Editing a task keeps the zone it was saved with; a task saved before the form had this picker usually shows **UTC**, and the form points out when a task's zone is not your own. Switch the picker and save to move it. A repeat can also end on its own, under **End repeat**: never, on a date (the end of that day in the repeat's time zone), or after a set number of runs. The form always previews the date of the next run; read it before launching. |
| **Recipients** | The email addresses that receive each run's result. You set them here rather than in the library prompt, so the same prompt can serve different audiences, and the form keeps them across an edit — including when you re-insert a different prompt from the library. (They are delivered as an instruction the form writes into the task's prompt for you. That only matters if you drive the API directly: a client that replaces a task's prompt wholesale replaces that instruction too.) |
| **Tools & files** | What the task may reach: mailboxes, connectors, files. Some connections are always on for every run; the rest are selected per task, so new tasks start with your deployment's recommended set and an existing task never silently gains new connections. Files can also be attached directly to the task, for work that runs against a fixed reference like a template or a lookup table. |
| **Context** | Notes that travel with the task for the people who operate it: why it exists, who owns it, what to do if it fails. These are shown to operators and never enter the assistant's instructions. Alongside them sit **tags** and the task's **persona**, which is left blank for the workspace default unless the task genuinely needs a different one. Tags are how you group related tasks: they show as chips on the board and it filters by them (see [Finding things](#finding-things)), so a tag you give a task here is a way back to the whole group later. |
| **Advanced** | Further settings, including the model the task runs on and an option for a recurring task to carry a short summary of its previous run into the next one. A new task is pre-filled with your workspace's default model (**GPT-5.6 Luna Pro** unless your admin has changed the default in Settings) and, by default, **DeepSeek V4.1 Flash** as its fallback; it keeps both unless you change them here, even if you never open Advanced. The model in particular is worth choosing deliberately: match it to the demands of the job rather than leaving it to chance. |

The model picker includes the deployment's configured **Workspace** models and
filters public-catalog suggestions against the active provider routes once they
load. Choose the provider-prefixed workspace entry when using a direct provider.
If a direct provider takes precedence over OpenRouter, the picker also offers
confirmed public-catalog models through explicit OpenRouter routes such as
`router/openai/gpt-4o`, where `router` is the deployment's configured provider
name. Pick that labeled **Workspace** row to deliberately use OpenRouter.
Its known catalog price remains available to **Estimate Cost**, including the
cost-ceiling warning. An unavailable custom default is not converted into an
OpenRouter model; choose an available model instead.
Saved task model values are not rewritten when provider configuration changes;
review the model in **Advanced** if a run reports that no provider serves it.

Some generated prompts include an **EXECUTION REQUIREMENTS (JSON)** block.
Keep it when copying the prompt. Fleet checks it when the run starts, before
model execution, and reports missing tools or sandbox network access. It does
not enable connections or permissions for you. A block that marks its tool list
as the run's whole roster limits the task to exactly those connector tools, and
a call to any other connector tool is refused. For file uploads, select
**Allow network egress** in Advanced; the administrator's network policy still
applies. Working mailbox or connector calls do not prove that shell uploads can
reach the destination. A required source must still be fetched and checked by
the running task. The block may also name the tools whose success means the
run is done. When one of them succeeds, the run finishes without the completion
verifier, and its log says which tool satisfied it. The self-audit still
applies.

**Estimate Cost**, beneath the form, produces a **cost forecast** on demand: the
token breakdown for the run you are describing and, where the model's pricing is
known, a dollar estimate with a range, plus a warning when the estimate would
exceed the deployment's cost ceiling. Nothing appears until you press it, and it
never blocks the submit — but pressing it is the cheapest moment to notice that
a daily job is about to be an expensive one.

## 4. Run states

Every status badge means one of ten things. What each one means, and what to do:

| Status | What it means, and what to do |
| --- | --- |
| `SCHEDULED` | Waiting for its time. Nothing to do. |
| `PENDING` | Due and queued for a free agent slot. Nothing to do; it will start on its own. |
| `LEASED` | An agent has claimed it and is starting it now. A brief, normal step between pending and running. |
| `RUNNING` | Executing now. Open it to watch live if you are curious. |
| `SUCCESS` | The run completed. Its result is in the logs, and in the recipients' inboxes if the task has any — a task with no recipients succeeds quietly, so a green badge is not by itself proof that anyone was emailed. |
| `ERROR` | A failure the scheduler could not route anywhere else — the uncommon one. Treat it like the row below: read it, fix the cause, rerun. A run that failed and is going to be retried does not sit here; it goes back to `PENDING` until its next attempt. |
| `DEAD_LETTERED` | Failed and set aside for review — the badge most failures end on. It means one of two things, and the row says which: retries were exhausted, or the failure was deterministic and was quarantined on its first attempt without retrying. This occurrence is done: read the failure, fix the cause, and replay it if you want this run again; see [When a run goes wrong](#6-when-a-run-goes-wrong). A recurring schedule still continues on its own — the next occurrence is queued unless the schedule has reached its configured end (its run count or end date). Two exceptions resume only on replay: chains parked by two consecutive dead-letters, and rows dead-lettered before that continuation shipped. A chain whose prompt has a malformed EXECUTION REQUIREMENTS line parks on its first dead-letter; replay reruns the same line, and editing the finished task only starts a one-off run, so create the task again with the corrected prompt and its schedule. |
| `CANCELLED` | A person stopped it. The record notes who. Deliberate stops never retry or alert. |
| `PAUSED_AWAITING_INPUT` | The run reached a decision it was not willing to make alone and stopped to ask a question. It holds no resources while it waits. Answer it and it resumes from there; this state is the system working as designed, not a failure. Most deployments let it wait indefinitely, but an operator can set an expiry after which an unanswered run fails — ask yours whether one is set. |
| `PAUSED_AWAITING_WAKE` | The run put itself to sleep until a set time or an expected event, for example to check back on something later. It wakes on its own; every sleep has a deadline. Sleeping runs also get their own short list on the dashboard, so parked work stays visible without a status filter. |

> **The retry rule in one line.** Transient failures retry themselves — quietly,
> by going back to the queue with a growing delay. Deterministic ones do not:
> they go straight to `DEAD_LETTERED` on the first attempt, because a rerun
> would fail the same way until something changes. So a failure sitting in
> `DEAD_LETTERED` has not necessarily burned through retries; it may never have
> been retried at all, on purpose.

## 5. Everyday actions

### Run a task on demand

**Run now** queues an immediate run. Use it after fixing a prompt, or when
someone needs today's report ahead of schedule. It does not disturb the task's
regular schedule. It is offered on a task that is waiting or finished, not on
one that is already in flight or paused — those rows want a stop, an answer, or
their wake time instead.

### Open a task's record

Click any row to open the task's record: the full transcript of what the run did,
its tool calls and results, token and cost figures, and the final output. A task
that has not run yet has none of that — its record opens on its details and
**No transcript available for this task**, and the actions that read a
transcript (**Discuss in chat**, **Download logs**) wait until there is one.
The same line appears when a transcript exists but your permissions do not
cover reading it, so it says "available" rather than claiming none was ever
written. The
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
a notification goes out if your deployment has a channel configured for one —
not every deployment does, so if you are waiting on a run, the board is the
thing that always knows. Provide the answer there and the task re-queues and
continues with your answer in hand. A paused task consumes nothing while it
waits, and by default it waits as long as it takes — but a deployment can set an
expiry, after which an unanswered run is failed and its question cleared. If
yours does, an answer left for tomorrow may be an answer left too late; ask your
administrator whether there is a deadline.

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

**2 · Read the analysis.** A failed task usually carries an automatic diagnosis:
a classification of what went wrong and a suggested fix, attached to the failed
row. It is a convenience, not a guarantee — a deployment can switch it off, and
generating it is best-effort — so if the row has no analysis, move straight on
to the transcript rather than hunting for it.

**3 · Read the transcript.** The log shows every step the run took. If the task
retried, the record keeps each attempt; a picker in the log view switches between
the latest transcript and superseded ones, so an earlier failure is never papered
over by a later retry.

A failed run may already have completed an external action. Check the tool
results before rerunning it. Provider recovery can continue after a completed
tool step while retaining its results; it stops if a tool began in the failed
step and its outcome cannot safely be replayed. The completion verifier, when a
fallback model is configured, checks repairs up to three times. Unresolved
verification does not count as success: after the third check that still finds
something missing, Fleet stops with a “completion verification unresolved”
error. The transcript retains completed actions and partial work; it does not
mean a successful publish or send was undone. If the verifier itself cannot be
reached (a timeout, a provider failure), Fleet retries it once. When the run's
self-audit passed and its critical actions went through, with none failing, the
run then succeeds, and its message starts with `[completion_unverified_verifier_error]`.
That flag means nobody double-checked the result, so review it before relying
on it. A verifier that answers with something that is not a verdict, or with
nothing at all, still counts as an unsuccessful check, as does an outage on a
run that performed no critical action.

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
| **Keep the ledger** | Stop tasks that should no longer run; reserve deletion for things that should leave the record entirely — the history of what ran and how it went is the most useful thing on this screen. Do not expect it to last forever, though: deployments prune old run logs on a schedule (by default, terminal runs older than 90 days once a task has more than its ten most recent), so anything you need as a permanent record should be exported rather than left on the board. |
