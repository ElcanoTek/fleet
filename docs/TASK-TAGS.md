# Task tags on the board

What shipped when tags stopped being write-only, what deviated, and what was
deliberately left out. The user-facing description lives in the Operations
Center guide ("Finding things"); this note is for whoever changes the code.

## The gap

Tags (#212) were storable and queryable but invisible. The create form accepted
them, `models.Task` carried them, `TaskFilter.Tags` filtered on them,
`GET /tasks?tag=a&tag=b` narrowed to tasks carrying **both**, and
`GET /tasks/tags` returned the whole catalogue with per-tag counts — and no
surface in the web app ever rendered a tag again. So the one thing a tag is
for, finding the rest of its group, could not be done from the UI at all.

The gap was found while writing the user guide, which is worth recording: the
guide had to describe tags as "stored metadata rather than a control on that
screen", and a sentence that awkward is usually a defect wearing prose.

## What shipped

- **Chips.** A task's tags render on its table row and its phone card, coloured
  from the same hashed palette (`shared/lib/labelColors`) as the chat
  conversation labels, so one tag reads the same everywhere it appears.
- **Every chip is a control.** Clicking one adds that tag to the board's
  filter; clicking a selected one removes it. Tags AND server-side, so each
  addition narrows and each removal widens.
- **A Tags group in the filter bar** — a select that *adds* a tag, plus a
  removable chip per selected tag. The select never holds a value: the board is
  filtered by every chip beside it, not by the last one chosen, and a select
  reading `ops` while `ops + urgent` were applied would misstate the board.
- **Tags count as an active filter**, so **Clear filters** appears and clears
  them. Without that the only way back to the full board was a page reload.
- **`/api/orchestrator/tasks/tags`**, a thin proxy to the existing catalogue
  endpoint. The static `tags` segment wins over the sibling `[taskId]` route,
  so it does not shadow `GET /tasks/{id}` — the same ordering `cmd/fleet/main.go`
  spells out explicitly for the Go router.

Two things underneath had to change:

- **`passThroughQuery` forwards every value of a repeated parameter.** It read
  only the first, which is right for every single-valued filter and wrong for
  `tag`: dropping the second of `?tag=a&tag=b` *widens* the result instead of
  narrowing it — the one direction a filter must never fail in. Single-valued
  parameters behave exactly as before.
- **The phone card's box moved from its `<button>` to the enclosing `<li>`.**
  The chips cannot live inside the card button (see below), so they render as
  its sibling; moving the border, radius and background one level out is what
  keeps them inside the visible card.

## Two decisions worth keeping

**A chip is a `<button>`, and on the phone card it is NOT inside the card
button.** The card is itself a `<button>`, and a control nested inside a button
has invalid accessibility semantics however it is marked up — assistive
technology can expose only the outer "View task" control, or make the tag
action ambiguous. The first version dressed the chip as a `<span
role="button">`, which dodges the HTML parsing rule and keeps the actual
problem. Siblings, not children. `TasksTable.test.tsx` pins this.

(The table row is also `role="button"` and already nests real buttons — run
now, delete. That predates this change and was left alone rather than widening
the PR; it is a reasonable thing to revisit.)

**The catalogue has a TTL, not a fetch-once and not a fetch-every-reload.**
`GET /tasks/tags` is a `GROUP BY` over every task's tag array. Fetching it with
the dashboard's 30s refresh would pay for that constantly to catch a list that
changes only when somebody retags something; fetching it once per activation
left a tag created later, on a task not on the current page, unreachable until
a full reload, because `active` stays true for the whole signed-in session.
`TAG_CATALOGUE_TTL_MS` (5 minutes) bounds both. The gap it leaves is closed
from the other side: `tagOptions` is the catalogue **unioned with the tags on
the listed tasks**, so a brand-new tag is selectable the moment a task carrying
it appears, without waiting for the refresh.

## Honest scope

- **The catalogue is deployment-wide; the board is not.** A non-admin sees only
  their own tasks, so a tag a colleague uses can appear in the dropdown and
  filter down to nothing. This is the same pre-existing property as the
  dashboard counters, it is not introduced here, and the guide states it rather
  than hiding it.
- **No tag counts in the UI.** The catalogue returns them, but "ops (12)" above
  a board showing two of them is a number that is wrong for most readers, for
  the reason above. Names only.
- **Not shipped:** tag management from the board (renaming or deleting a tag
  across tasks — retagging is per-task, through the form or
  `POST /tasks/{id}/tags`), and no tag filter on the Upcoming or Sleeping
  panels.
