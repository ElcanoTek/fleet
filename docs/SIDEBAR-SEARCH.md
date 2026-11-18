# Search in the sidebar looks everywhere

The rail's search field (`⌘K` / `Ctrl K`) used to filter one list: the
person's own chats, shown inside the Chats section. Three things followed, and
people read each of them as "my chats are gone":

- **Team chats were never searched.** The Team chats section kept showing its
  whole list, unfiltered, whatever was typed.
- **A folded section hid its results.** With Chats folded, the title matches
  were rendered into a hidden container. With Team chats folded (and some
  workspaces share every chat with the team), a search for a teammate's chat
  showed nothing at all.
- **A hidden section hid its results for good.** Hide Team chats or Projects
  in the Customize panel and nothing in them could be found again from the
  rail.

This note records what shipped to fix that, and what was left out.

## What shipped

- **One list of results, in place of the sections.** While the search field
  holds text, the Projects, Chats and Team chats sections (and Archived) step
  aside for a single list. Clearing the field puts the rail back exactly as
  it was: the order, names, hidden and folded choices in the layout are not
  touched by searching (`ConversationSidebar.tsx`, "Search results").
- **Search ignores what the sidebar shows.** Results come from every list the
  page holds: the person's own chats (pinned, temporary, and filed in a
  project), teammates' shared and multiplayer chats, projects, and archived
  chats. Whether a section is hidden or folded makes no difference.
- **Results are grouped under the section they come from**, each with a
  count: Chats, Team chats, Projects, Archived, and In messages. Groups use
  the person's own section names where they renamed them. The order is
  fixed, own chats first.
- **A result from a hidden section says so.** The group carries the line
  "Hidden in your sidebar. Search still finds what is in it." and a
  **Show it** button that un-hides that one section in the person's layout
  (saved like any Customize edit). The button is offered only to an account
  that may change its sidebar (`useCanCustomize`): a read-only Viewer, or an
  account whose layout has not loaded, sees the line without it.
- **Matching** (`conversationOrganization.ts`: `searchTokens`,
  `matchesSearch`): every word typed must appear, in any order, ignoring case
  and accents. A chat or project is matched on its name; a teammate's chat
  also on its owner's email, so typing a colleague's name finds their chats.
  The matched words are marked in each title. The same rule now backs the
  label-plus-title filter (`filterConversations`).
- **A result says where it lives.** An own chat filed in a project shows
  `Project ›` before its title; a patrol chat carries its patrol mark (as it
  does in every list: [SIDEBAR.md](SIDEBAR.md), "Row marks"); an archived message
  hit carries **Archived**.
- **In messages** (`GET /api/search`, Postgres full text) covers the
  person's own chats, archived ones included, and the chats shared with them
  (see "Searching shared chats" below). It is deduplicated against chats
  already listed by title. A shared hit shows the team glyph and its owner's
  name and opens as its Team chats row does. While that
  answer is on its way, an otherwise empty list says "Searching…" rather than
  "Nothing matches". Hits belong to the query typed now: the previous
  query's hits are dropped as soon as the text changes.
- **Search says what it could not see.** Team chats, projects and archived
  chats are each read on their own after the page boots. Until a list has
  loaded (or if its read failed), the results end with "Not searched, because
  it has not loaded: …" naming it, and the empty state does not claim to have
  looked everywhere. The archived list is not read when the page boots, so
  the first search reads it (`chat-experience.tsx`).
- **The empty state says where search looked** (all chats, team chats,
  projects, archived, hidden and folded sections included) and has a
  **Clear search** button that returns focus to the field.
- **Keyboard.** `↓` in the field moves to the first result, `↑`/`↓` walk the
  results, `↑` on the first returns to the field, and `Enter` in the field
  opens the first result. A row's `⋯` menu stays reachable with `Tab`.
- **Screen readers.** The list is a labelled region; a polite status line
  announces the number of results for the query.

## Searching shared chats (server)

`GET /search` used to read only the caller's own conversations. It now also
searches the conversations other people shared with the caller
(`store.SearchConversations`):

- **The shared set is the Team chats list's own.** Search calls
  `ListSharedAndMultiplayerConversations` for the caller and searches inside
  the ids it returns: team-visible to the caller's team, or Multiplayer the
  caller may take part in; never archived, sealed or deleted. There is no
  second copy of that rule in the search query, so the two cannot drift: a
  chat the Team chats list would not show cannot be found, and a share that
  is revoked is gone from search on the next query. Because the list is read
  before the query runs, every shared result is checked against it a second
  time before it is returned: a share revoked while a query is in flight is
  dropped, not returned.
- **Only what a teammate may read is matched.** In a shared conversation the
  query matches the title and user/assistant text, the rows
  `teamTranscriptEntry` passes. Tool calls and reasoning were never indexed.
  A compaction summary is indexed for its owner, but a team reader gets it as
  a content-free boundary, so it is neither matched nor quoted in a preview
  for anyone but the owner. This narrower rule is applied to Multiplayer
  chats too.
- **A shared result carries `user_email`**, the owner's; the caller's own
  results omit it. No other field of the conversation is returned.
- Cost: one extra list read per search. No migration, no new index.

`internal/store/fts_test.go` pins this: found by a teammate by title and by
text; not by another team or a person on no team; a never-shared chat stays
private; unsharing removes it; summary text and tool calls are not matched or
quoted; archived and Multiplayer-excluded chats are not found; and every
shared result is inside the caller's shared list.

## Deviations and limits
- **Projects keep their tree rows in the results** (expand, the `⋯` menu,
  drop target), rather than a flat link, so a project found by search behaves
  like the one in the section.
- **A label filter on its own** (no text) still shows only the person's own
  chats carrying the label, as before. With text too, the labels narrow the
  own and archived results; team chats and projects carry no labels of the
  viewer's, so they are matched on the text alone.
- **`j`/`k` list navigation** walks the own-chat results only, as before
  (`visibleConversationOrder`); the arrow keys above walk every result.
- **Shared chats in a project the caller has not opened** are found like any
  other shared chat; the result opens the chat, not the project.
- **Not done:** searching background tasks from this field, ranking across
  groups, and remembering recent searches.

## Tests

- `conversationOrganization.test.ts` — the matching rule and the marked ranges.
- `ConversationSidebar.search.test.tsx` — every group, folded and hidden
  sections, **Show it**, the patrol tag, message-hit dedup, the empty state,
  and the arrow keys.
- `e2e/mocked/search.spec.ts` — the same paths in a browser against the Go
  gateway, with an axe pass over the results.
