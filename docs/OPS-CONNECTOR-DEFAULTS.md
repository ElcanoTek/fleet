# Operations connector defaults

## Problem

Chat and Operations both expose connector controls, but only Chat honored a
bundled optional connector's `enabled_by_default` setting. Operations rendered
every connector off. This was especially confusing when a client converted a
previously hidden connector into a visible, default-on option: new Chat
conversations started with it enabled while new scheduled tasks did not.

## Shipped behavior

For a new Operations task, catalog entries returned with `enabled: true` start
selected and are saved in the task's explicit `mcp_selection`. Entries returned
with `enabled: false` remain off. Remote hosted connectors keep their existing
auto-available behavior and are not copied into the bundled selection.

The catalog loads asynchronously. Until the user changes the picker, newly
arrived defaults are reflected in the form. The first user change becomes an
explicit override, so a later catalog refresh cannot overwrite it.

Editing is deliberately different: an existing task always displays and saves
its persisted selection. A later manifest-default change never silently widens
an existing task's connector access.

Completed tasks cannot be rewritten; saving their editor resubmits a new
one-off copy. The resubmit carries the connector picker's complete visible
selection, including an explicit empty list, so connector changes apply to the
new run instead of silently inheriting the completed source task's old list.

### Always-on connectors

`mcp_selection` contains optional additions only. At run binding, Fleet unions
that list with every enabled non-optional bundle connector. Selecting an
optional reporting connector therefore cannot remove a mandatory mailbox or
other always-on capability. An empty selection means "always-on connectors
only," not "all configured optional connectors."

The Operations picker includes always-on connectors as locked informational
rows. Their state comes from the Manager's live MCP discovery catalog:

- **Always on** means the connector exposed tools and will be added to every
  permitted run.
- **Unavailable** means the connector is configured as always-on but exposed no
  live tools. The row is unchecked and visibly marked unavailable; the UI never
  paints it healthy just because the manifest says it is enabled.

Always-on is not an authorization bypass. An explicit empty credential
allowlist still binds no connector, and credential/tool/persona gates can only
narrow what a run may call.

### Chat status parity

Chat's compact connector popover consumes the same live always-on status
contract. Available rows are shown as locked **Always on** entries; a discovery
failure is unchecked and marked **Unavailable**. The locked switch uses an
intermediate tint between an optional connector's off and selected states so it
does not imply that the user selected it.

Only status semantics are shared. Chat keeps its compact popover and its
per-conversation rules: optional bundled and hosted remote connectors remain
toggleable, account seats remain conversation-specific, and the toolbar badge
counts only selected optional connectors. Operations keeps the full-size task
picker, where hosted remotes are auto-applied to scheduled runs. Neither chat
endpoint persists an always-on row into `enabled_optional`; runtime availability
continues to come from the independent non-optional roster.

## Scope and deployment

This is generic Fleet UI behavior. Which connectors are visible and default-on
remains entirely bundle-owned through `optional: true` and
`enabled_by_default: true`. No client names or connector names are hardcoded in
Fleet.

This change does not rewrite existing task rows. Operators must edit an existing
task once to adopt a newly default-on optional connector. Existing empty
selections continue to require no row migration: under the task-selection
contract they receive the always-on set and no optional additions.

## Verification

Component coverage proves that new tasks select and submit default-on bundled
connectors, asynchronously loaded defaults appear, a user can replace the
default selection, and existing tasks do not inherit later defaults. Runtime
coverage proves optional choices are unioned with always-on connectors in both
broker and compatibility paths, remote-only seat pins do not remove that set,
and an explicit deny-all still binds nothing. Catalog and picker coverage pins
the active/unavailable status display. Chat API and composer coverage pins the
same live status, its locked intermediate switch state, and the optional-only
persistence/badge boundary.
