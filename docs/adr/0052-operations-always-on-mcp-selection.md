# ADR-0052: Operations MCP selections add optional servers without replacing always-on servers

- **Status:** Accepted
- **Date:** 2026-09-01
- **Deciders:** fleet maintainers (issue #1333)

## Context

The shared picker models `mcp_selection` as a list of optional connector
choices. Operations previously passed a non-empty list to the scheduled binder
as an exact bundle-server set. That silently removed non-optional connectors
which were absent from the picker because the product described them as always
on. A task selecting SSP APIs could therefore lose its mandatory mailbox even
though the same mailbox remained available in Chat.

Hiding mandatory connectors also made failures invisible: the form could not
distinguish an intentionally non-selectable connector from one that failed MCP
discovery.

## Decision

1. A scheduled task's `mcp_selection` is the set of optional bundle additions
   (plus remote seat pins, which the remote overlay consumes). It is not an
   exact replacement for the bundle runtime.
2. Before opening either a broker scope or the in-process compatibility client,
   the runner unions every enabled non-optional bundle server into that task
   selection. An empty selection therefore binds always-on servers only.
3. Explicit credential deny-all remains stronger than the union and opens an
   empty scope. Tool allowlists and persona policy remain later narrowing gates.
4. The Operations catalog includes always-on servers as locked informational
   rows. `enabled` on those discriminated rows is derived from live tool
   discovery; a configured connector with no discovered tools is rendered
   **Unavailable**, not permanently checked.
5. Optional `enabled_by_default` remains a form-initialization policy. It is
   persisted as an explicit optional selection and does not redefine always-on.

## Consequences

- Selecting optional connectors can no longer remove a mandatory connector.
- Broker and compatibility scheduled paths use the same union rule and both use
  per-run clients, so task identity and connector ledgers do not fall back to a
  shared deployment workspace.
- A zero-tool MCP server is treated as unavailable in the Operations status
  view. This is deliberately a usable-tool signal, not a full transport health
  probe.
- Chat behavior is unchanged; its existing scoped selection already unions
  non-optional servers.

## Enforcement

- `internal/scheduledrun/scheduledrun_test.go` pins empty, explicit optional,
  remote-only, and deny-all selections.
- `cmd/fleet/mcp_broker_runtime_test.go` pins Optional metadata across broker
  startup and reload.
- `internal/agent/mcp_optin_test.go` pins discovery-derived always-on status.
- `McpServerPicker.test.tsx` and `TaskCreateModal.test.tsx` pin locked status,
  selection independence, unavailable rendering, and optional-only persistence.
