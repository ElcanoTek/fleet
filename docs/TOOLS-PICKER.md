# Chat Tools picker: search, categories, statuses and the library

The composer's Tools menu (the wrench) used to be one long list of every
connector with a switch, each row about 150px tall. With a dozen or more
connectors it took scrolling to find one. This change gives it the design's
quick search, static category headings and a full library modal, and gives
each connection a category and one of five statuses.

Source: the Claude Design file "Tools Picker" (Tools Popover, Tools Library
Modal) for the visuals, and the "Tools Picker Updates: Build Summary" for
behaviour and copy.

## What shipped

**Popover** (`web/src/app/chat/ui/ToolsPopover.tsx`). Search pinned at the
top and focused on open ("Search tools…"), with the expand icon beside it
that opens the library. The header reads "4 on", or "9 results · 4 on" while
searching; that count includes always-on connections. While browsing, an
**On** section comes first. It is a snapshot taken when the popover opens, so
a row stays put while you switch it. After it come static category headings
(no collapsing), with Misc Tools last and empty groups hidden. Rows are about
64px with a two-line description. Search matches name, description, category
name and the connector's tool names, and highlights what matched. Rows rank
name-prefix > name-contains > category > description > tool name. Groups
order by their best match, and the top result is the active row. A row that
matched only on a tool shows it under the count (`↳ ix_clone_deal`). **All
on / All off** cover the toggleable rows on screen: with a search they say
how many ("All 3 on") and the tooltip says "Applies to 3 connections shown".
The keys are `↑ ↓` to move, `Space`/`Enter` to switch, and `Esc` to clear,
then close. With no matches it reads "No tools match ‘xyz’" and offers "Open
the full library" and "Clear search".

**Library** (`ToolsLibrary.tsx`): the Prompt library's sheet (portalled,
full-bleed on phones, `useDialogA11y`). The header shows "Tools &
connections" and "N of M on", where M counts every connection whatever its
status, plus "Manage connections in Settings" for admins and close. The rail
has search (same rules), Categories (All, each category with a count, Misc
last) and Status filters (On, Off, Always on, Needs setup, Unavailable) whose
counts follow the category and search. The middle is the list (no card view),
with headings in All and a bulk bar ("Applies to 10 connections" / "… in
SSPs" / "… shown"). The right is the detail pane: category kicker, name, a
large switch, the status line, the full description, "Manage in Settings",
the account menu, the state panel, and a quiet "Show N tools".

**Statuses** (`toolsPicker.ts` is the one place the rules live, and is unit
tested):

| Status | Control | Popover (browsing) | Popover (search) | Library |
| --- | --- | --- | --- | --- |
| On / Off | switch | shown (On section / its category) | shown | switch |
| Always on | lock, counts as on | On section | shown | "Always on in every chat." |
| Needs setup | none | hidden | dimmed; "Set up ›" (admin) / "Needs admin setup" | warn panel |
| Unavailable | none | hidden | dimmed, "Unavailable" | "Unavailable right now. Try again later." |

A dimmed search row opens that connection in the library.

**House tools.** The `house` category holds the deployment's own tools
(for Elcano: Lens, Pages, Keel, Deal Sheet and Deal Planner) and is listed
first, after On, so they stand out. Fleet carries no client name: the bundle
names the group with `branding.house_tools_name` ("Elcano Tools"), and
without it the name is "<app_name> Tools".

**Server side.** `mcp_servers` entries take `category:` (an ID from
`internal/clientconfig/connector_categories.go`; see docs/MCP-CATALOG.md,
"Category"). An unknown ID fails the load, and an untagged entry lands in
Misc Tools with a boot warning. `/mcp-servers` and
`/conversations/{id}/mcp-servers` add `category`, `status`
(`available | needs_setup | unavailable`) and `default_account` to each row,
plus the ordered `categories` list. A manifest connector whose
`enabled_env`/`enabled_groups` are not satisfied used to be dropped from
every surface. It is now listed as `needs_setup`, with the manifest tool
allowlist as its tool names. It never launches and is never an opt-in
candidate. An optional connector that started with no tools, or an always-on
one discovery could not reach, is `unavailable`. `/mcp-catalog` lists
needs-setup connectors with `needs_setup: true`, so Settings → Connections
shows them as a "Needs setup" card. Each bundled card has the anchor
`#connector-<name>` that the library links to.

**Account menu.** The per-chat seat choice (#988) stays on the popover row
(a compact pill) and in the detail pane, now a styled menu instead of a
native select. The default option reads as the user's default seat from
Settings → Connections when they picked a named one. Otherwise it reads as the
deployment's `branding.app_name` ("Elcano"), or "Primary login" for a hosted
connection. Settings → Connections' own default option reads the same way.

## Deviations from the design

- **Row click toggles.** The design only lets a dimmed row open the library
  on click. Here a toggleable row's click also switches it, as the old picker
  did, so the hit target is the whole row. The switch is still the
  keyboard-reachable control.
- **Toolsets** (the design's "OPTIONAL IDEA" screen, unreachable in the mock)
  are not built.
- **Copy for admins on the Settings card.** Connector credentials are
  environment variables on the server; there is no page in the app to enter
  them. "Set up in Settings" therefore lands on the connector's card, which
  says the credentials are added on the server by whoever deploys the
  workspace. It does not name the missing credential, which would need a
  per-connector plain-words hint in the manifest.
- **Unavailable has no reason.** Discovery does not record one, so the copy
  is the design's generic sentence.
- **Tokens.** The design is on Elcano Design System 2.0; this is built from
  the app's own tokens (`--color-*`). The design's new components (switch,
  compact search, small pill, status kicker, group header, match highlight,
  warn/deny panels) live in `ToolsPickerParts.tsx` and the two surfaces, not
  in a shared design-system package.

## Deferred

- Toolsets (saved groups of connections).
- A credentials page so an admin can set a connector up in the app.
- Per-deployment category overrides (rename, reorder, hide).
- The orchestrator task modal's picker (`McpServerPicker`) is unchanged.
- Tagging every bundle's connectors and removing their "Appears once X is
  set" text is done in each bundle's own repo, after the fleet release that
  knows `category` ships (additive-first schema evolution).
