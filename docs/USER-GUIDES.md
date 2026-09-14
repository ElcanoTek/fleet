# The user guides: /help and the `fleet-guide` skill

fleet had no end-user documentation. Everything in `docs/` is written for the
people who build and operate fleet; nothing in the product explained fleet to
the person typing into the composer. This page records what shipped to close
that gap, how the one document reaches two readers, and what was deliberately
left out.

## What shipped

Two guides, written for users rather than contributors:

| Guide | Covers |
| --- | --- |
| **Chat** | The rail, transcript and composer; asking well; picking a model; getting data in (attachments, connectors, shared files); approval cards; memory and projects; sharing, branching and compacting; the prompt library; turning a conversation into a scheduled task; triage when a turn goes wrong; keyboard shortcuts. |
| **Operations Center** | Reading the board and its counters; the anatomy of a task; every run state; running, editing, answering and rating a run; the five-step path when a run goes wrong; conventions for a shared board. |

They reach users two ways, from one source:

- **`/help`** — a surface of the app, in the same NavRail/PageTopBar chrome as
  Chat, the Operations Center and Settings, with a **Guides** item in the rail on
  every surface. The guides render as Markdown with a per-guide contents rail;
  both themes and any bundle's palette follow, because the styles
  (`.guide-prose` in `globals.css`) speak only in semantic tokens.
- **The `fleet-guide` built-in skill** — the same two Markdown files, shipped in
  the binary's skills pack (`internal/clientconfig/builtin_skills/fleet-guide/`),
  so the assistant answers "how do I make this run every Monday?" from the text
  the user would have read, and can then offer to do it.

The second is the point of the first. A user with a question is already in a
conversation; making the product's own documentation something the assistant can
quote is worth more than a page they have to find.

## One document, two readers

`internal/clientconfig/builtin_skills/fleet-guide/*.md` is the source of truth.
`web/src/app/help/guides/*.md` is a verbatim copy, refreshed by
`make sync-guides`, and `scripts/check_guides_sync_test.go` fails CI on any
drift in either direction (a web-only guide the assistant cannot read is a
failure too).

The copy is not laziness — neither direction of linking works:

- `go:embed` cannot reach outside its own package, so the skill's files must
  live under `internal/clientconfig/`.
- Turbopack refuses to build through a symlink that leaves the Next project root
  ("Symlink … points out of the filesystem root") the moment a server module
  reads through it, so `web/src/app/help/guides` cannot be a link to the skill.

So: two real files, one generator, one test that makes drift loud. The failure
this guards against is specific and bad — an assistant confidently citing a page
the user is not reading.

## The guides were reviewed against the code, not just imported

The drafts described the product accurately but had gaps and a few claims that
had drifted. Corrected while porting:

- **A tenth run state.** `LEASED` (an agent has claimed the task and is starting
  it) is a real, user-visible badge; the draft listed nine states. Added, along
  with the rule the board actually enforces: a live run can only be stopped —
  run-now, edit and delete are withheld until it finishes
  (`web/src/app/orchestrator/TasksTable.tsx`).
- **Expiry is a default, not a law.** Unpinned conversations are swept after
  `CONVERSATION_TTL_DAYS` (14), and a per-user cap
  (`CONVERSATION_UNPINNED_CAP`, 50) can evict the oldest before the clock runs
  out. The draft stated 14 days flatly and omitted the cap.
- **`⌘F`** was missing from the shortcut table (it opens search, and is
  suppressed while typing so the browser's own find still works there).
- **Two card classes** were missing: the display-only email-preview card, and
  notify mode, where a card records what was done rather than gating it
  (docs/APPROVAL-CARDS.md). The cards section's headline promise — nothing
  happens until you answer — is now scoped to approve-mode cards up front, since
  notify mode is a genuine exception to it and a reader who stops after the
  first paragraph should not be misled.
- **Datasets is not admin-only**; SLA, Usage and Adoption are
  (`orchestrator-client.tsx` render-guards exactly those three).
- **The cost forecast** under the task form was undocumented.
- **Server time is not UTC.** `ServerClock` renders the deployment's configured
  zone and names the task-scheduling default in its tooltip when the two differ;
  task-row timestamps render in the *browser's* zone. The draft asserted one UTC
  clock for all three, which is exactly the mistake that makes someone read a
  schedule wrong.
- **Run now is not on every row.** `RUNNABLE_STATUSES` excludes the two paused
  states as well as the in-flight ones — a paused run needs its answer or its
  wake, not a second copy.
- **Paused runs can expire.** `FLEET_PAUSED_TASK_EXPIRY_MINUTES` is 0 (disabled)
  by default, but where an operator sets it `ExpirePausedTasks` fails an
  unanswered run and clears its question. The draft promised "no rush"
  unconditionally.
- **Tags do not filter the board.** The API takes `?tag=`, but `TaskFilters`
  offers only status, creator, scheduled-only and text, so the guide describes
  tags as stored metadata rather than a control on that screen.
- **`ERROR` is not where a failure waits for you — `DEAD_LETTERED` is.**
  `handleRunFailure` re-queues a retryable failure (back to `PENDING`, with
  backoff), quarantines a deterministic one on its *first* attempt, and reaches
  `ERROR` only when the re-queue itself fails. So `DEAD_LETTERED` does not imply
  exhausted retries, and the draft's neat transient-vs-deterministic split was
  attached to the wrong badge.
- **`SUCCESS` means completed, not delivered.** Recipients are optional, so a
  task with none succeeds quietly.
- **Three more things that are conditional, not guaranteed:** the paused-task
  notification (a deployment with no channel configured has a no-op notifier),
  the automatic failure analysis (`FLEET_ERROR_ANALYSIS_ENABLED`, and
  best-effort even when on), and the assistant having read these guides at all
  (`skills_builtin: false`, `skills_hidden`, or a bundle skill winning the name
  — `/help` renders either way, which is exactly why its copy now hedges).
- **Deleting a task does not free a title.** The unique column is `name`, which
  the in-app form never populates; the visible Title is the non-unique `title`.
  The draft's "frees its name for reuse" (inherited from the delete button's own
  tooltip) invited someone to destroy a run history to reuse a label, which the
  same guide's "Keep the ledger" convention tells them not to do.
- **De-branded.** The drafts were written for one deployment ("Elcano · Fleet",
  "your Elcano contact"). fleet is the engine, branding arrives in a bundle, and
  a deployment may be white-labeled — so the guides name no product at all (they
  say "the platform" and "your administrator") and describe deployment-specific
  things — personas, connectors, model names and cost bands — as things a
  deployment supplies.

## A product defect the guides now warn about

Writing the fix-in-the-library routine down surfaced a real bug, and it is
documented rather than fixed here because fixing it is a change to the task form
that deserves its own PR:

**Re-inserting a library prompt on a task silently drops its email recipients.**
`TaskCreateModal` delivers recipients by appending a `CRITICAL ACTION` block to
the task's prompt (`buildFinalPrompt`), initializes its `emails` state to `[]`
in edit mode rather than parsing them back out, and `PromptLibrary`'s `onInsert`
replaces the whole prompt. So the routine both guides recommend — fix the
library prompt, re-select it on the task, save — produces a task that runs
correctly and emails its report to nobody, with no warning.

The guides now carry the extra step (re-enter the recipients before saving, then
**Run now** once to confirm the mail arrives) in all three places that routine
appears. The real fix is to parse the recipients back out of the prompt when the
form opens, or to stop storing them in the prompt at all; that is a follow-up.

## Keeping them true

A user guide rots differently from a design note: nobody re-reads it, and when
it is wrong it does not fail a build — it just tells someone to press a button
that is not there. Worse here than in most repos, because the same text is what
the assistant quotes back when a user asks. Three things keep it honest, in
descending order of reliability.

**1. Mechanical checks, where a claim can be checked.** Prose does not compile,
but some of it can still be pinned:

| Check | What it prevents |
| --- | --- |
| `scripts/check_guides_sync_test.go` | The two copies becoming a fork — the assistant citing a page the user is not reading. |
| `scripts/check_shortcuts_doc_test.go` | A shortcut table naming a chord the shell does not bind. This one is not hypothetical: `docs/keyboard-shortcuts.md` advertised `Mod`+`F`/`N`/`J` long after the app stopped binding them, and that stale table was copied into the guide in good faith. The authority is the help-overlay catalog in `chat-experience.tsx`. |
| `web/src/app/help/headings.test.ts` | An in-guide cross-reference pointing at a heading that no longer exists. |
| `internal/clientconfig/builtin_skills_fleet_guide_test.go` | The skill citing a guide file that moved, and the guides losing the sections the skill's description promises. |

Add a row when a new claim is checkable. The pattern to look for is a guide
sentence that restates a constant, an enum, or a list the code already owns.

**2. The convention in `AGENTS.md`.** A change to anything a person can see or
press updates the guide in the same PR, then `make sync-guides`. Two habits make
that cheap rather than a chore: describe only what shipped, and write "by
default" for anything an operator can configure — most of what went wrong in
review was an unqualified absolute, not a wrong fact.

**3. Review.** Both rounds of review on the PR that introduced these guides
found real inaccuracies the drafts had carried in — a status badge that meant
something other than what was written, counters that count the deployment while
the table shows only your own rows, several "always" claims that were
deployment-configurable. That is the expected yield for a document of this kind,
and a reason to read a guide diff as carefully as a code diff rather than waving
it through as "just docs".

## Honest scope

- **The guides describe fleet as it ships.** A deployment can hide features,
  ship its own connectors and personas, and change windows like the retention
  period. The guides say so where it matters, and the skill instructs the agent
  to name deployment-set numbers as defaults and to say "I don't know" rather
  than invent a control.
- **`/help` is not gated on the skill.** The pages are built into the web app, so
  they still render for a deployment that sets `skills_builtin: false` or hides
  `fleet-guide`. The reverse is also true: hiding the skill removes the
  assistant's copy, not the pages.
- **No search, no per-section deep links from elsewhere in the app.** The
  contents rail and the browser's own find are what a two-page guide needs. If
  the guides grow, search is the first thing to add.
- **The recipient bug above is documented, not fixed.** A docs PR is the wrong
  place to change how the task form round-trips recipients.
- **Nothing links into the guides contextually yet** — no "what does this mean?"
  affordance next to a status badge or an approval card. That is the obvious
  next step and was deliberately not bundled into this change.
- **The guides are engine documentation.** Per the repo boundaries doctrine they
  describe fleet itself; a deployment that wants to document its own connectors
  or protocols ships a skill in its bundle, which wins a name collision with the
  built-in pack.
