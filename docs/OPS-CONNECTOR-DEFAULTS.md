# Operations connector defaults

## Problem

The shared connector picker is used by Chat and Operations, but only Chat
honored a bundled optional connector's `enabled_by_default` setting. Operations
rendered every connector off. This was especially confusing when a client
converted a previously hidden connector into a visible, default-on option: new
Chat conversations started with it enabled while new scheduled tasks did not.

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

## Scope and deployment

This is generic Fleet UI behavior. Which connectors are visible and default-on
remains entirely bundle-owned through `optional: true` and
`enabled_by_default: true`. No client names or connector names are hardcoded in
Fleet.

This change does not rewrite existing task rows. Operators must edit an existing
task once to adopt a newly visible connector. It also does not change the
scheduled runner's legacy meaning of an empty `mcp_selection`; that compatibility
behavior remains tracked separately from picker defaulting.

## Verification

Component coverage proves that new tasks select and submit default-on bundled
connectors, asynchronously loaded defaults appear, a user can replace the
default selection, and existing tasks do not inherit later defaults.
