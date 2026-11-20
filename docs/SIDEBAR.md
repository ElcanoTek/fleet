# A sidebar of your own

People use fleet in very different ways: as a chatbot, as a scheduler of
background tasks, as one big multiplayer patrol for a whole team, or as a
dashboard a fully autonomous agent keeps up to date and nobody chats to. This
note records what the sidebar is, how each person (and each workspace)
arranges it, how it works, and what was left out on purpose. User-facing
behaviour is in the chat guide
(`internal/clientconfig/builtin_skills/fleet-guide/chat.md`), which `/help`
renders.

The sidebar was redesigned in November 2026 from the owner's mockups. The
aim was a calmer, tidier sidebar: one that scrolls as a whole, is the same on
every page, and says less on each row. "What shipped" below describes it as
it is now; "Where it differs from the design" and "Left out" record the
choices made along the way, and "Back-end changes" lists every server change
the redesign needed.

## What shipped

### One sidebar, on every page

- **One frame, one scroller** (`NavRail.tsx`). The brand row, then search
  with the pen and the **+**, then one scrolling body (`rail-body`) that
  holds the top items, Pinned, Projects and Chats together, then the account
  menu. No list inside the body is a scroll box of its own, at any width;
  before, Projects and Team chats were capped boxes and the chat list scrolled
  alone. The scrollbar is thin and shown on hover or keyboard focus, with its
  gutter reserved (and given back from the right padding) so rows sit as far
  from the right edge as from the left on every platform.
- **The same sidebar on every page.** Chat, Background tasks, Patrols,
  Analytics, Settings and Guides all draw `ConversationSidebar` inside
  `NavRail`, so a person always sees their pinned chats, projects and chats
  and one click takes them back to any of them. Chat feeds it from its own
  state. Every other page renders `AppSidebar`, which reads the same lists
  itself (`useSidebarLibrary`: `GET /api/conversations`, `?archived=true`,
  `?scope=shared`, `/api/projects`, `/api/conversations/order`,
  `/api/server-config`), starting from what the last page showed so moving
  between pages does not flash an empty sidebar. Page changes are client-side
  navigations, so the lists survive them.
- **What happens where.** On a page that is not Chat, opening a chat or a
  project navigates to Chat (`/chat?c=`, `/chat?team=`, `/chat?project=`). A
  change that is one request is made in place, optimistically, and read again
  if it fails (pin, rename, labels, the hand-set order, a project's pin and
  name); a failure is said in the sidebar. Anything that needs one of Chat's
  dialogs or confirmations (Share, Download, Transfer, a chat's patrols, a
  background task, a workflow, delete, archive, move to a project, a new
  project, Select, **Delete temporary chats**) is handed to Chat
  (`chatHandoff.ts`): the page notes the action in a module variable, goes to
  Chat, and Chat carries it out once with the same code a click in its own
  sidebar runs. A full page load starts the note empty, so a reload never
  deletes a chat.
- **The brand row is the live one.** The logo (`brandLogoSrc`, a deployment's
  own from its bundle), the eyebrow and the name are drawn exactly as before
  the redesign. The row gains a gear (Customize) beside the collapse toggle.
- **Drag the edge to resize.** The sidebar's right edge is a focusable
  separator (`rail-resize`). Dragging sets the width (240 to 480 px, and never
  more than 45% of the window); from the keyboard, the arrow keys step it,
  Home and End go to the limits, and Enter or a double-click puts it back to
  the default 300 px. The width is remembered in `localStorage`
  (`rail-width`): it is about this screen, like the collapsed state, not about
  the account. No handle on a touch screen, in the icon strip, or while the
  sidebar is an overlay.
- **The icon strip.** Collapsed (from 640px up) the sidebar is the logo, the
  expand button under it, the **+**, and the top items; the account avatar
  stays at the bottom. Search, the pen, the gear and the lists come back with
  the sidebar (`Ctrl`/`⌘` + `K` still expands it onto the search field).
  Each icon's name is shown beside it on hover or keyboard focus, drawn on
  the page rather than inside the scroller so it is never clipped.
- **On a phone** the sidebar is an off-canvas drawer with the full sidebar in
  it, scrolling as one, and the keyboard manners of a modal drawer (focus
  moves in, Tab stays in, Escape closes, focus returns to the button).
- **Dashboard view keeps the strip.** Dashboard view used to hide the
  sidebar. From 640px up it now keeps the icon strip beside the board, so
  there is always a way back to a chat; the strip's expand button slides the
  whole sidebar in over the board with a scrim. That slide-over is a modal
  drawer: focus moves into it, Tab stays inside, Escape closes it (and only
  it, not the view), and focus returns to what opened it. It does not change
  the person's own collapsed or expanded choice, which is for the chat view.
  On a phone the dashboard header keeps its **Show sidebar** button.

### What it lists

- **Search, the pen and the +.** Search is one field (`Ctrl`/`⌘` + `K`); the
  pen starts a chat; the **+** opens a menu: **New chat**, **New patrol**
  (the guided wizard), **New background task**, then **New project** and,
  where sealed chats are available, **New sealed chat**. On Background tasks
  the task item opens that page's own form; elsewhere it goes there with the
  form open (`/orchestrator?new=1`, consumed once). On Background tasks an
  account that cannot create tasks is not offered the item.
- **The top of the sidebar** (`RailNav.tsx`): **Patrols** and **Background
  tasks**, then whatever the person keeps there: a dashboard, a chat, a
  project, a background task, or a page (Guides, Settings, and Analytics for
  administrators). There is no "Chat" row: starting a chat is the pen. A
  chat kept at the top opens as a chat or as a **dashboard**
  ([DASHBOARD-VIEW.md](DASHBOARD-VIEW.md)), and a patrol chat's row shows its
  next check. Background tasks shows a quiet count while tasks are running.
  Items can be moved, renamed, given another icon, or hidden; icons come from
  a fixed set of Flag sprite icons, or are any short emoji.
- **Pinned** is one section: pinned projects, the person's pinned chats, and
  the teammates' chats they pinned, in one order they set by hand
  (**Reorder**: drag, or the arrows). The saved order names a chat by its id
  and a project as `project:<id>` ([CHAT-ORDER.md](CHAT-ORDER.md)). A pinned
  project is listed here and not again under Projects.
- **Projects** lists the first eight, then **All N projects** opens the rest
  in place; the open project is never behind that fold. A project's chats
  fold under it.
- **Chats** is the person's own unpinned, unfiled chats and, by default, the
  teammates' chats shared with them, in one list. Chips above it narrow it
  to **All**, **Mine**, **Shared** or **Patrols**, and each chip appears only
  when it would change the list. The list is grouped by date (**Today**,
  **Yesterday**, **This week**, **Older**, by the reader's own calendar days)
  under the two date sorts; an A to Z list has no groups. The heading's menu
  holds the sort (Recent activity, Newest first, Title A to Z), **Filter by
  label**, **Select chats…** and **Delete temporary chats…**. Folding or
  hiding Chats does not affect search: while the search field holds text the
  sections step aside for one list of results drawn from all of them, hidden
  and folded ones included ([SIDEBAR-SEARCH.md](SIDEBAR-SEARCH.md)).
- **Teammates' shared chats, three ways.** In Chats (the default), in a
  **Team chats** list of their own, or not listed (search still finds them).
  The layout stores the list of their own as `separate_lists: true` and the
  hidden state on the `team` section, so a layout that says nothing is one
  list. Hiding them keeps which list they were in.
- **Rows say one thing** (`RowStatus.tsx`). A row is its title and at most
  one status at its right edge, the first of these that applies: a reply
  being written; the days left for a temporary chat inside the warning window
  ([CHAT-RETENTION.md](CHAT-RETENTION.md)); a patrol's next check ("19h") or
  **Paused**; **Live** (Multiplayer); shared with the team; shared by link;
  sealed. A teammate's chat also carries its owner's initial. The status is
  decoration: the sentence it stands for goes to a screen reader as the row's
  description and to the tooltip. Everything else a chat can be stays off the
  row: labels are in the tooltip, the `⋯` menu and **Filter by label**, and a
  temporary chat with time left shows nothing. **Status on rows** in Customize
  turns the marks off (the descriptions stay). Which chats are patrols comes
  from one read of `GET /api/patrols` per page, shared by every part that
  shows them; what the page itself knows of the open chat's schedules wins
  over it, so a new patrol is marked without a reload.
- **Row menus.** Rows put their actions behind a `⋯` button that takes the
  status's place on hover or keyboard focus; on a touch screen it is always
  shown and each tap box is at least 44px. A chat's menu, in order: **Pin**,
  **Move to the top**, **Rename**, **Move to project**, **Labels** | **Set
  as start page** | **Add a patrol…** (or **Patrols…**), **Make a background
  task…**, **Save as workflow…** | **Share…**, **Download chat…**,
  **Transfer ownership…** | **Archive**, **Delete…**. A project's: **Open
  project**, **New chat**, **Reorder chats** | **Pin**, **Move to the top**,
  **Rename**, **Set as start page** | **Share…**, **Project settings…** |
  **Delete project** (a member who is not the owner gets the first group and
  the two sidebar items). A teammate's chat is not the viewer's to rename,
  file, archive or delete: its menu holds **Pin**, **Move to the top**, **Set
  as start page**, **Copy link**, and what the API lets that person do
  (**Download chat…** for a Multiplayer participant, **Transfer ownership…**
  for an admin). **Move to the top** keeps a target among the top items;
  **Pin** is the other thing, a place in the Pinned section.

### Patrols is a page

- **`/patrols`** (`patrols/page.tsx`, `PatrolsView.tsx`) replaces the
  Patrols dropdown. It lists every patrol the person set up, across their
  chats, then **Shared with you**: the patrols teammates run in chats shared
  with them. A row is the one a chat's Patrols dialog shows, plus its chat:
  the name, the schedule, the run count, the approval mode, when it checks
  next, **Run now** and pause or resume. A patrol whose runs are being
  skipped behind an approval card says so once, in a chip.
- **Opening a row** reads `GET /api/patrols/{id}` and shows the approval mode
  (the chat's setting, changed here; Autonomous asks for the same acceptance
  as everywhere), **Recent runs**, what each run is asked to do, **Edit
  schedule & task**, **Open chat** and **Remove patrol** (a second click
  confirms).
- **A teammate's patrol is there to read.** It shows whose it is, when it
  runs and what recently happened, with one way into its chat. Only the
  person who set a patrol up can run, pause, edit or remove it (the server's
  rule), and its instructions are sent only to them.
- **Starting one.** **New patrol** opens a form: **Runs in** a new chat
  (made and named for the patrol, and taken back out if the patrol cannot be
  armed) or an existing chat (added beside the patrols already there; a
  sealed chat is not offered), then the task, the schedule, the approval mode
  and a label. **Use guided setup** opens the wizard
  ([PATROL-LAUNCHPAD.md](PATROL-LAUNCHPAD.md)). With no patrols the page
  offers **Set up your first patrol** (the wizard) and **Or write one
  yourself** (the form). "Could not load" is never shown as "you have none".

### Customize

- **The gear** in the brand row opens a small panel (`CustomizePopover.tsx`)
  with the choices people change most: **Fleet opens to**, **Density**
  (Comfortable or Compact), and **Show** switches for Patrols, Background
  tasks, Projects and teammates' shared chats. **More options** opens the
  full panel. Every change is the real thing: it shows in the sidebar at once
  and saves to the account a moment later.
- **The full panel** (`SidebarCustomizer.tsx`, `SidebarEditor.tsx`) is a
  *non-modal* sheet: the sidebar stays visible and usable beside it. It also
  opens from the account menu (**Customize sidebar**). It holds **Fleet opens
  to**; **Start from** (Classic, Just chat, Dashboards first, Automation,
  Team room: a preset arranges the top items and the lists, and keeps pins,
  shortcuts, the start page, density and the row options); **Top of the
  sidebar** (order, name, icon, show or hide, **Add**); **Pinned** (the
  sidebar's own Pinned section: reorder, unpin, **Pin**; a person's own, so
  not in the workspace editor); **Lists** (order, name, fold and show or hide
  for Pinned, Projects and Chats; where teammates' shared chats go; **Group
  chats by date**; **Status on rows**); and **Density**. An administrator
  sees **My sidebar / Workspace default**; the second goes to **Settings →
  Admin → Sidebar**.
- **The start page** (once "Home") is where fleet opens. It is the last chat
  the person had open until they choose one: a top item, something pinned, or
  any chat, dashboard or project from its `⋯` menu (**Set as start page**).
  It does not have to be a top item: one that is not is kept in the layout as
  a hidden shortcut. It applies only when a page load *opened fleet*: the
  address was `/chat` (where `/` and sign-in land) with no parameter but
  `lang`, and only once per page load (`home.ts`). A link to a chat, a team
  link, a project link or any in-app navigation never goes there. A start
  page that is the person's own chat opens in the boot's place; a teammate's
  chat, a project, Background tasks, a task or a page opens once the boot
  settles, pages with `location.replace` so Back does not return to a `/chat`
  that would bounce again. The layout read is awaited for at most 900 ms; a
  slower read opens the most recent chat as before.

### Kept from before the redesign

- **Pinning a teammate's chat.** A teammate's chat's `⋯` menu has **Pin** /
  **Unpin**. The pin is the viewer's own: `chat.shared_conversation_pins`
  (migration 0204) holds one row per person and chat, and
  `conversations.pinned` — the owner's pin, which is what the retention
  sweep reads — is not touched. So nothing changes for the owner or for any
  other teammate, and a viewer's pin never keeps a chat alive. `POST
  /api/conversations/{id}/pin` sets the chat's own pin for its owner and the
  caller's pin for anyone it is shared with (`store.SetSharedPin`, which
  accepts only a chat `ListSharedAndMultiplayerConversations` returns for the
  caller; any other ID is the 404 an unknown ID gets). In the shared listing
  (`?scope=shared`) and in a non-owner's read of the chat, `pinned` is the
  caller's pin; **the owner's pin is no longer sent to teammates**, and it no
  longer orders their list. A pinned teammate's chat moves to the
  **Pinned** section, keeps its own row (status, owner's initial, `⋯`), and
  shares the section's hand-set order with the person's own pinned chats and
  pinned projects ([CHAT-ORDER.md](CHAT-ORDER.md)), whether teammates' chats
  are in Chats or in a list of their own. A person who hid teammates' chats
  sees none of them, pinned or not. A Viewer account, whose
  writes the API refuses, is not offered the item. A pin on a teammate's
  chat is refused (409) once the person's Pinned group — their own pinned
  chats and these pins together — holds 500, the most one saved order names;
  pinning one's own chat is not capped, as before. Pins are taken one at a
  time per person, so two at the cap cannot both land. A pin row is tied to
  its account and its chat by foreign keys, so it goes with whichever is
  deleted, and transferring a chat clears every such pin on it. A pin on a
  chat that stops being shared with the person stays (shared again, the chat
  comes back pinned) until they next pin something, when such rows are
  retired so they neither use up room nor return past the cap. Unpinning an
  ID that is neither pinned by nor shared with the caller is the same 404.
  The team listings (`?scope=team`, a project home's Team list) report the
  caller's own pin too, never the owner's. Not done: a
  pinned teammate's chat is not walked by `j`/`k` and cannot be selected,
  like every teammate's row.
- **Temporary has one definition.** `store.temporaryConversationClause` is
  what the retention sweep deletes for inactivity, what the per-user cap
  evicts, and what **Delete temporary chats** removes: unpinned, in no
  project, not archived, not shared by link or with the team, not
  Multiplayer, and with no armed or paused patrol or timer. The web client
  mirrors it in one predicate (`isTemporaryChat`,
  `deleteAllUnpinnedScope.ts`) used for the days-left status, the confirm
  dialog's count and the local update after the delete. Two things changed to get
  there. **Delete temporary chats used to delete every unpinned, unfiled
  chat**, including a chat shared with the team, a Multiplayer chat other
  people are in, and a chat with a patrol; it now keeps those, as the sweep
  always did, and its confirmation says so. And the owner's chat listing
  carries `has_schedule` (an armed or paused patrol or timer), so a chat
  with a timer that was never opened on this page is not marked temporary.
  The period quoted in the sidebar is the deployment's
  (`conversation_ttl_days` in `GET /api/server-config`, from
  `CONVERSATION_TTL_DAYS`), not a fixed number. While teammates' chats are in
  Chats, folding or hiding Chats folds or hides them with it; search still
  finds them ([SIDEBAR-SEARCH.md](SIDEBAR-SEARCH.md)). Deliberately
  unchanged: a project's chats stay under their project (a project chat is
  listed only there), a chat or project kept at the top is listed there and
  not again below, `j`/`k` walk the person's own rows, and select mode
  selects only their own.
- **Read-only accounts.** The API refuses a Viewer's writes, so the gear's
  panel, the editor, **Reset**, **Move to the top** and **Set as start page**
  are offered only once `GET /api/me` has confirmed another role (the rule
  chat reordering uses). A Viewer can open both panels and see their sidebar,
  with every control disabled and the reason said.
- **The workspace default** is edited in **Settings → Admin → Sidebar** with
  the same editor, on a draft, saved with **Save for everyone**. It is stored
  on the server, not in the client-config bundle, so it changes without a
  deploy. **Start from my sidebar** copies the admin's own layout as a
  starting point. Until the saved default has loaded the editor and every
  write are disabled; a failed read offers **Try again** and never lets an
  admin save over a default they have not seen. The editor is also locked
  while a save is in flight, so no edit is lost to the save's answer.

## Where it differs from the design

The mockups were a direction, not a specification. These are the places the
build chose differently, and why.

- **The brand row, the account button and the account menu are unchanged.**
  The mockups drew a switcher beside the name and a name over the email in
  the footer; fleet has one workspace per deployment and no display names
  (only emails), so neither was built, and the logo and name are the live
  ones.
- **The icon strip is smaller than drawn**: the logo, the expand button, the
  **+** and the top items. The mockup also put the gear, search, the pen and
  the pinned chats there.
- **Chips appear only when they change the list.** A person with no shared
  chats and no patrols sees no chip row at all.
- **Labels are off the rows**, and a temporary chat shows nothing until it is
  inside the warning window. Both follow from "one status when it matters".
- **Presets are text cards**, not miniatures, and keep density and the row
  options as well as pins and the start page.
- **"Fleet opens to" is a list in the gear's panel and a dropdown in the full
  panel**, where the list of chats can be long.
- **A patrol's countdown is one unit** ("in 19h"), with the exact time in its
  tooltip, and a skipped patrol says so once (the chip), not twice.
- **Background tasks keeps its page as it was**: the task list and everything
  under the header are untouched. **New task** is one button in the page
  header and an item in the sidebar's **+** menu; the sidebar's own New task
  button is gone.
- **From another page, some menu actions open in Chat** rather than in place
  (see "What happens where").

## Left out

- **A summary of each patrol run.** **Recent runs** lists what happened
  (ran on schedule, ran when asked, skipped and why, stopped) from the run
  history fleet already keeps. Quoting what a run found needs a link from a
  run to the reply it produced, which the schedule history does not record.
- **A teammate's patrol's instructions.** They are the creator's; the page
  shows the rest.
- **Changing a kept shortcut between Chat and Dashboard from the Pinned
  section.** The toggle is on top items only.
- **A workspace-wide Pinned list.** Pins are personal; the workspace default
  arranges the top items and the lists.
- **A display name on the account button** (see above).
- **Saving a section's folded state as you click.** As before, folding lasts
  for the page; "starts folded" is set in Customize.
- **Time zone on interval patrols.** "Every 2h" names no zone; a clock time
  does ("Weekdays 08:00 · America/New_York"). This is the existing rule.

## Back-end changes

Every server change the redesign needed. None changes a table: there is no
migration.

- **`GET /api/patrols` also lists patrols shared with the caller**
  (`store.ListSharedPatrols`): a patrol in a chat a teammate owns that is
  shared with the caller's team or that they take part in through
  Multiplayer, and a patrol a participant set up in the caller's own chat.
  Each entry now also carries `last_run_at`, `run_count`, `approval_mode`,
  `mine`, and for a teammate's patrol `owner_email`, `multiplayer` and
  `team_visible`; the caller's own active patrols carry `skipped` when runs
  are being held behind an approval card (`store.PatrolSkips`). The caller's
  own entries are unchanged otherwise, so an older client reads the list as
  before.
- **`GET /api/patrols/{id}`** (new; `handlePatrolByID`, mounted in the
  browser gateway) returns `{patrol, runs}`: the patrol as the list gives it,
  plus its `prompt` for its creator only, and up to 20 entries of its run
  history (`store.ListPatrolRuns`, from `sched.chat_schedule_runs`). A patrol
  that is neither the caller's nor shared with them is the 404 an unknown id
  gets.
- **The sidebar layout** (`internal/sidebar`) gains a `pinned` section, put
  first in a layout saved before it existed, and two optional flags,
  `status_off` and `dates_off`. `home` may now name a hidden shortcut (a
  start page that is not a top item); a hidden destination is still refused.
- **The pinned order may name pinned projects** (`store.SetChatOrder`): an
  entry `project:<id>` is kept when it names a pinned project the caller owns
  or that is shared with their team, and dropped otherwise, like a chat that
  has left the scope.
- **`/patrols` is a page** of the browser tier (`internal/webui/spa.go`),
  titled in English, Spanish and Portuguese.

## Storage and API

- **Effective layout** = the person's own ?? the workspace default ?? the
  built-in layout. The built-in layout lives only in the client
  (`BUILTIN_LAYOUT` in `layout.ts`); the server never invents one.
- `GET /api/sidebar` → `{"layout": <own|null>, "workspace_default": <ws|null>}`.
  `PUT /api/sidebar` (a layout) → `{"layout": <normalized>}`.
  `DELETE /api/sidebar` → back to the default. These sit behind the member
  gate and `mutate`, so a Viewer account reads but cannot save.
- **Saves are ordered by a revision number.** The browser numbers every edit
  after the server's stored `rev` (returned by `GET` and by every save) — a
  server-held sequence, never a device clock, so a device whose clock runs
  ahead cannot lock out another — and sends it as `rev` with a `PUT` (beside the layout's fields)
  or a `DELETE` (`{"rev": n}`). The server applies a save only when its `rev`
  is newer than the stored one and otherwise answers `"stale": true`, so two
  requests that overlap — a save still in flight when the page unloads and
  sends the next one with `keepalive` — can never leave the older layout on
  top. A save overtaken by another tab or device gets `"stale": true` with
  the layout the server keeps and its `rev`, and the page shows that layout
  instead of claiming its own was saved. A numbered reset keeps the row with the JSON value `null` and its
  `rev`, so a late older save cannot bring the old layout back. A save
  without `rev` (0) is unordered and always applies.
- `GET`/`PUT`/`DELETE /api/admin/sidebar` (admin-gated like
  `/api/admin/settings`) → `{"layout", "updated_by", "updated_at"}`. Saves and
  resets are logged with the admin's identity.
- **Tables** (migration 0202): `chat.sidebar_layouts` (one row per user,
  lower-cased email) and `chat.workspace_sidebar_layout` (one row, `id = 1`).
  `DeleteUser` removes the user's row in its transaction.
- **Validation** (`internal/sidebar`, mirrored by `normalizeLayout` in
  `layout.ts`): version 1; at most 60 items; item ids `^[a-z0-9][a-z0-9_-]{0,39}$`;
  kinds `builtin` (ids `chat`, `patrols`, `tasks`), `chat`, `project`, `task`
  (ref `^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`), `page` (ref `analytics`, `help`
  or `settings`); labels cleaned of control characters and at most 60 runes
  (40 for a section); titles cut to 120; an icon is an allowlisted sprite name
  or 1–8 non-ASCII runes; `view: "dashboard"` only on a chat; missing builtins
  and sections are appended (`pinned` first, the others last); `status_off`
  and `dates_off` are kept only when true; a `home` naming nothing usable
  (missing, a hidden destination, or Patrols) is cleared, while a hidden
  shortcut may be the start page. A stored row that no longer validates reads as
  absent, with a warning logged, rather than failing the page. The body is
  capped at 64 KiB.
- **Nothing in a layout is an authority.** A shortcut is a link: opening it
  goes through the same API calls and access checks as opening the target any
  other way. A chat or project pinned in the workspace default is shown only
  once it is known that this person can open it, on every page: Chat checks
  the lists it holds, and the other pages read the same lists once
  (`useFallbackTargets`). One in the person's own layout that they can no
  longer open is shown faded, labelled "not available to you", so they can
  unpin it.
- **The workspace default carries no private names.** `GET /api/sidebar`
  serves it to members with every item's captured `title` dropped
  (`sidebar.Layout.ForMembers`); `/api/admin/sidebar` keeps them for the
  editor. A title is a snapshot of the target's own name taken in the admin's
  browser, so an admin's pin to their own chat never names it to anyone who
  cannot open it; the rail names a workspace pin from the member's own lists.
  An admin's `label` is kept: it is text written for everyone.
- **Private for you, shared for the team.** Your own sidebar can pin any
  chat, dashboard or project you can open, private ones included; it is
  yours alone. The workspace editor offers only what the team can see — a
  chat shared with the team or in multiplayer (yours or a teammate's), a
  project shared with a team, and pages — and flags a pin already in the
  default that is not shared ("Not shared with the team"), since it shows
  only to the people who can open it.
- **No Analytics in the workspace default.** It is an admin page:
  `CheckWorkspace` refuses it, the workspace picker does not offer it, it is
  never a workspace Home, and the rail shows an Analytics pin only to admins.
- **No background tasks in the workspace default.** Task visibility is
  owner-scoped for everyone but admins, so `PUT /api/admin/sidebar` refuses a
  `task` item (`sidebar.CheckWorkspace`), the workspace editor's picker does
  not offer tasks, and "Start from my sidebar" leaves them out. A person may
  still pin their own tasks in their own layout.
- **Home goes only where this account can go.** A chat or project Home from
  the workspace default is followed only once the member's own lists confirm
  they can open it (within the same short wait); a person's own Home is
  skipped when it is known to be gone. A task is never a workspace Home.

## The client store (`store.ts`)

- One module-level copy per page, shared by every rail (chat, background
  tasks, settings, guides, analytics) and the panel through
  `useSyncExternalStore`.
- The last effective layout is cached in `localStorage`
  (`fleet.sidebar.layout.v2`), tagged with the signed-in account's email from
  the page bootstrap, so a page load paints the person's own rail at once
  instead of flashing the default; the server answer replaces it. Another
  account on the same browser never reads it (nor is sent to its Home). If the
  server cannot be reached (or predates this feature), the rail renders the
  built-in layout, not the cached one, and the panel says changes cannot be
  saved.
- Nothing can be edited until the person's saved layout has loaded: until
  then the rail shows a cached or built-in stand-in, and an edit on it would
  be saved over the real one. If the read fails, the panel says so and offers
  **Try again**; the quick pins stay hidden meanwhile.
- Edits apply at once and are saved 450 ms after the last one (one PUT per
  burst). Saves run one at a time, in order, each sending the layout as it is
  when its turn comes, so an older save can never land after a newer one. An edit still waiting when the page is hidden for good (a reload, a
  closed tab, a shortcut that loads another page) is sent on `pagehide` with
  `keepalive`. A read that started before an edit never overwrites it. A failed
  save keeps the edit on screen and offers **Try again**.
- `ConversationSidebar` provides `SidebarTargetsContext` on every page: live
  titles, which chat or project is open, whether a target is still available,
  the Pinned section (for Customize), and, on Chat, in-place opening (no
  reload). Other pages navigate to `/chat?c=`, `/chat?project=` (a
  consumed-once link that opens a project's home) or `/orchestrator?task=`.
- The collapsed state and the dragged width are this browser's
  (`localStorage`: `rail-collapsed`, `rail-width`), not the account's.

## Deliberately not done

- **No arbitrary links.** Shortcuts point at fleet's own chats, projects,
  tasks and pages only. An external URL in a layout an admin can push to
  everyone is a phishing surface for little gain.
- **No per-team defaults.** One workspace default; teams can share a layout
  by having one person copy theirs into the admin page. Revisit if asked.
- **Section open/closed state is not saved as you click.** Folding a section
  in the rail lasts for the page; "starts folded" is set in the panel. Saving
  every click would turn people who follow the workspace default into people
  with their own layout without their deciding to.
- **No custom icons.** The start page is Flag's map pin and hiding is a
  switch: the sprite has no house or eye, and product icons come from Flag
  only.
- **The top bar is not customizable.** It carries a page title and the page's
  own actions; nothing in it was asked to move.
- **A 400 from `PUT /api/sidebar` is shown as a generic "Couldn't save"**
  rather than the server's validation message, which is English-only. The
  editor's own checks prevent every case a person can reach through the UI.

## Tests

- `internal/store/chat_schedules_test.go`: patrols shared with the caller
  (team-shared, Multiplayer, a participant's patrol in the caller's own
  chat), one patrol with its run history, and the prompt kept from anyone but
  its creator. `internal/httpapi/patrol_draft_test.go`: the list and the 404.
- `internal/store/chat_order_test.go`: pinned projects in the pinned order.
  `internal/sidebar/sidebar_test.go`, `internal/httpapi/sidebar_test.go`: the
  validation table, the `pinned` section, the row options, methods, shapes,
  400s and the admin gate. `internal/store/sidebar_layout_test.go`: round
  trips, the workspace row, and that deleting a user deletes their layout.
- `internal/store/shared_pins_test.go`, `internal/httpapi/shared_pin_http_test.go`:
  a pin on a teammate's chat is per viewer, is refused for a chat not shared
  with the caller, joins the pinned order, and is cleaned up with the account
  and the chat.
- `web/src/app/shared/sidebar/*.test.ts(x)`: the layout normalizer, edits,
  presets and the start page (`layout.test.ts`); the store's precedence,
  debounced saves and when the start page applies (`store.test.ts`); the one
  row status and its order (`RowStatus.test.tsx`); the hand-off to Chat
  (`chatHandoff.test.ts`); the lists other pages read and the changes made in
  place (`useSidebarLibrary.test.tsx`); the same sidebar on another page
  (`AppSidebar.test.tsx`); the gear's panel and the editor
  (`CustomizePopover.test.tsx`, `SidebarEditor.test.tsx`); the patrol store
  (`targets.test.tsx`).
- `web/src/app/shared/ui/NavRail.sidebar.test.tsx`: one scroller, the brand
  row, the icon strip, and resizing by pointer and keyboard.
- `web/src/app/chat/ui/ConversationSidebar.*.test.tsx`,
  `chat-experience.*.test.tsx`: Pinned and its mixed order, the chips, date
  groups, the Chats menu, the **+** menu, the row menus and search.
- `web/src/app/patrols/PatrolsView.test.tsx`,
  `web/src/app/launchpad/patrolWizard.test.tsx`: the Patrols page and the
  wizard's host.
- `web/e2e/mocked/sidebar-redesign.spec.ts`: a large workspace in light and
  dark, on a desktop and a phone, with an axe scan; the menus; the gear; the
  Patrols page; the same sidebar on Background tasks; resizing; dashboard
  view. `chat-list.spec.ts`, `sidebar-layout.spec.ts`, `rail.spec.ts` and the
  other mocked specs cover the lists, a saved layout, the start page, the
  workspace default and the admin page.
