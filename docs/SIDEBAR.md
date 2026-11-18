# A sidebar of your own

People use fleet in very different ways: as a chatbot, as a scheduler of
background tasks, as one big multiplayer patrol for a whole team, or as a
dashboard a fully autonomous agent keeps up to date and nobody chats to. The
left rail used to be the same for all of them. This note records what shipped
to let each person (and each workspace) arrange it, how it works, and what was
left out on purpose. User-facing behaviour is in the chat guide
(`internal/clientconfig/builtin_skills/fleet-guide/chat.md`, "Customizing your
sidebar"), which `/help` renders.

## What shipped

- **The default is unchanged.** With no saved layout the rail renders exactly
  what it did: Chat, Patrols and Background tasks with their subtitles and
  test ids, then Projects, Chats and Team chats. Nobody's rail moves until
  they (or an admin) change it.
- **Destinations and shortcuts in one ordered list** (`RailNav.tsx`). The
  three built-in destinations can be moved, renamed, given another icon, or
  hidden. Next to them a person pins shortcuts: a chat, a patrol (a chat), a
  teammate's shared or multiplayer chat, a project, a background task, or a
  page (Guides, Settings, and Analytics for administrators). A chat shortcut
  opens as a chat or as a **dashboard** ([DASHBOARD-VIEW.md](DASHBOARD-VIEW.md)).
  Icons come from a fixed set of Flag sprite icons, or are any short emoji.
- **Home.** One item can be Home: the place fleet opens to. It applies only
  when a page load *opened fleet*: the address was `/chat` (where `/` and
  sign-in land) with no parameter but `lang`, and only once per page load
  (`home.ts`). A link to a chat, a team link, a project link or any in-app
  navigation never goes Home, so clicking Chat still opens Chat. A Home chat
  of the person's own opens in the boot's place (instead of the most recent
  chat); a teammate's chat, a project, Background tasks, a task or a page
  opens once the boot settles, pages with `location.replace` so Back does not
  return to a `/chat` that would bounce again. The layout read is awaited for
  at most 900 ms; a slower read opens the most recent chat as before.
- **The chat page's sections.** Projects, Chats and Team chats can be
  reordered, renamed, hidden, and set to start folded. Team chats are a
  section of their own in every order (never inside the Chats list), so
  folding Chats does not hide them. Hiding Chats keeps the **+** (and the
  sealed-chat lock) so a new chat is always one click away.
- **Compact** hides the one-line subtitles.
- **The Customize panel** (`SidebarCustomizer.tsx`, `SidebarEditor.tsx`) is a
  *non-modal* sheet: the rail stays visible and usable beside it, so every edit
  shows in the real rail as it is made. It opens from the account menu
  (**Customize sidebar**) and from a small **Customize** pill on the divider
  under the destinations (shown on hover or keyboard focus). It offers five
  presets (Classic, Just chat, Dashboards first, Automation, Team room), each
  drawn as a miniature of the person's own rail; a preset rearranges the
  destinations and sections and keeps every shortcut.
- **Quick pins.** **Add to sidebar** / **Remove from sidebar** in a chat's menu
  and a project's menu. A team's shared or multiplayer chat is pinned one at a
  time from the Customize panel's **Add** picker; Team chats rows carry no pin
  button of their own (it cluttered every row on a phone). A chat last
  read as a dashboard is added as one. A shortcut to the chat already open
  applies the view it names (dashboard or chat) rather than toggling.
- **Row menus.** Own chats, projects and Team chats rows put their actions
  behind a `⋯` button; an Archived row keeps its two buttons (**Unarchive**,
  **Delete**) on the row. With a mouse these controls appear on hover or
  keyboard focus; on a touch screen (`any-pointer: coarse`) they are always
  shown, since there is no hover to reveal them, and each tap box and the row
  are at least 44px (`hit-area-touch`, `any-pointer-coarse:min-h-11`). A Team chats row is a
  teammate's chat, not the viewer's to rename, file, archive or delete, so
  its menu holds **Add to sidebar** and **Copy link**: the team link
  (`/chat?team=<id>`) for a chat shared with the team, the chat link
  (`/chat?c=<id>`) for one reached only through Multiplayer (a team link
  refuses it), and no Copy link otherwise. When the clipboard refuses, the
  menu stays open with the link shown to copy by hand. Not done: a long-press
  to open a row's menu.
- **Read-only accounts.** The API refuses a Viewer's writes, so the editor,
  **Reset** and the quick pins are offered only once `GET /api/me` has
  confirmed another role (the rule chat reordering uses). A Viewer can open the
  panel and see their sidebar, with every control disabled.
- **Dashboard view keeps the rail in reach.** Dashboard view hides the rail;
  its header now has a sidebar button (in place of the decorative mark) that
  slides the rail in over the board with a scrim, so a person whose Home is a
  dashboard can move between boards without leaving the view. The rail is a
  modal drawer there: focus moves into it, Tab stays inside, Escape closes it
  (and only it, not the view), and focus returns to the button.
- **The workspace default** is edited in **Settings → Admin → Sidebar** with
  the same editor, on a draft, saved with **Save for everyone**. It is stored
  on the server, not in the client-config bundle, so it changes without a
  deploy. **Start from my sidebar** copies the admin's own layout as a
  starting point. Until the saved default has loaded the editor and every
  write are disabled; a failed read offers **Try again** and never lets an
  admin save over a default they have not seen. The editor is also locked
  while a save is in flight, so no edit is lost to the save's answer.

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
  and sections are appended; a `home` naming nothing usable (missing, hidden,
  or Patrols) is cleared. A stored row that no longer validates reads as
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
- On the chat page, `ConversationSidebar` provides `SidebarTargetsContext`:
  live titles, which chat or project is open, whether a target is still
  available, and in-place opening (no reload). Other pages render the title
  saved with the shortcut and navigate to `/chat?c=`, `/chat?project=` (a new,
  consumed-once link that opens a project's home) or `/orchestrator?task=`.

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
- **No custom icons.** Home is Flag's map pin and hiding is a switch: the
  sprite has no house or eye, and product icons come from Flag only.
- **The top bar is not customizable.** It carries a page title and the page's
  own actions; nothing in it was asked to move.
- **A 400 from `PUT /api/sidebar` is shown as a generic "Couldn't save"**
  rather than the server's validation message, which is English-only. The
  editor's own checks prevent every case a person can reach through the UI.

## Tests

- `internal/sidebar/sidebar_test.go`: the validation and normalization table.
- `internal/store/sidebar_layout_test.go`: round trips, the workspace row,
  and that deleting a user deletes their layout.
- `internal/httpapi/sidebar_test.go`: methods, shapes, 400s, the admin gate.
- `web/src/app/shared/sidebar/layout.test.ts`, `store.test.ts`: the
  normalizer, edits, presets, the store's precedence, debounced saves, the
  read/edit race, and when Home applies.
- `web/e2e/mocked/sidebar-layout.spec.ts`: the default rail, a saved layout,
  Home on open (and a link winning over it), the workspace default, the
  Customize panel end to end (with an axe scan), the chat menu's **Add to
  sidebar**, and the admin page.
