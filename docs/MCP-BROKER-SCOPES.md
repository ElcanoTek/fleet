# MCP broker scoped sessions

## Shipped scope

The internal `mcpbroker` transport can open an isolated per-run session in the
credential-owning process. `Client.OpenScope` sends only public configuration:
named server/account selections, a task ID, and a workspace path. The broker
returns an opaque scope ID and that scope's public tool catalog. Calls through
the returned `Scope` implement the existing `agentcore.MCPBroker` interface and
carry the opaque ID back to the broker; credential values are never protocol
fields.

Scope calls share the broker connection's existing request correlation and
cancellation path. A scope close waits for calls still active through that
`Scope`, is locally idempotent after success, and is retryable after a transport
or backend failure. Open, call, and close backend panics produce the same
value-free, incident-correlated response as unscoped broker operations, without
stopping the server. Backends that do not implement `ScopedBackend` retain the
old unscoped behavior and reject scope requests explicitly.
If cancellation races a successful open, the client retains the pending
correlation in a background cleanup and closes any late-arriving scope ID; the
caller still receives cancellation promptly, but the only resource handle is not
discarded.

The `fleet mcp-broker` backend implements that interface. It resolves account
suffixes, identity-routing refusal, `${FLEET_TASK_ID}`, and
`${FLEET_WORKSPACE}` inside the child; owns one MCP client per opaque scope; and
serializes scope close against calls already admitted to that client. Closing
the broker after its protocol loop exits also reaps every scope a disconnected
parent left open. Disabled servers are absent from the shared spawn-definition
map, so a stale selection cannot launch one through either the broker or the
legacy scheduled binder.

The protocol also supports credential-owner reload. The parent sends an empty
`reload` request — never resolved server definitions — and the child re-reads
its bundle and environment-backed connector configuration, applies the same
minimum add/remove/restart diff as the in-process client, and returns only the
public summary and refreshed tool catalog. Reload requests use the existing
correlation, cancellation, and value-free panic-containment machinery. Scope
opening serializes with reload: an opening scope receives a coherent old or new
base catalog, while scopes already open keep their original client until close.
Inline `http_tools` remain boot-pinned, matching the existing reload contract.

## Why this is separate

Scheduled runs can select different credential accounts and carry different task
identity/workspace values. A single process-wide MCP client cannot represent
those choices safely. The scoped protocol is the prerequisite that lets the
broker process construct and own a distinct client for each run while the agent
loop sees only the existing governed call seam.

## Deliberately deferred

The protocol, child backend, and Manager injection seam are implemented, but
production startup does **not** yet inject them. Its current options therefore
still build the credentialed client in the main fleet process. Scheduled runs
and per-user remote MCP credentials are not covered by these scopes. Issue #167
remains open until production startup and both run paths use the subprocess,
parent credential material is scrubbed, and the remote-MCP credential path is
moved behind an equivalent process boundary.

No authorization boundary is added here. Account allowlists, task policy, tool
approval, and audit remain responsibilities of the existing governed runtime;
the scope transports the already-authorized selection and cannot widen it.

## Driver integration

The interactive driver accepts an injected `MCPBroker` and public `MCPCatalog`
on `TurnConfig` and threads them into the same `agentcore.Run` call used by the
local-client path. Its per-user remote-MCP overlay composes with either base: a
remote server name routes to the short-lived user client, while every bundle
server routes to the injected broker.

`ManagerOptions` can now supply that broker/catalog plus a per-turn scope opener.
Broker injection requires an explicit public catalog (an explicit empty catalog
is valid), and a scope opener is rejected unless the base broker is also set;
miswiring cannot silently construct a local credentialed fallback.
For every interactive turn, Manager sends all enabled mandatory bundle servers
and only the conversation's enabled optional bundle servers, carrying the
user's public default account names and the already-bound conversation workspace
path. Synthetic native toggles and per-user remote servers are excluded from the
bundle selection. The returned scope broker/catalog drives the unchanged
governed loop and remote overlay composition, and Manager closes the scope with
a fresh bounded context even when the turn context was cancelled. A scope-open
failure fails the turn closed before provider or tool execution; it never falls
back to the local client.

The local-client default remains for transitional callers and tests. Production
startup does not inject the new Manager options yet. The child-side reload
protocol is ready, but Manager's reload adapter and production broker injection
remain part of the switch-on sequence.

The scheduled `Agent` accepts the same broker/catalog pair and threads it into
its existing `agentcore.Run`; governed sub-agents inherit that pair, and the
per-user remote overlay composes over it. Broker mode does not advertise the
in-process `mcp_load_servers` mutation tool because it has no mutable local
client.

The scheduled `Runner` can now inject a per-task scope opener. An explicit task
selection is preserved exactly; a selection-less task maps to every enabled
bundle server on its default seat, matching the shared-client catalog it used
before. The opener receives public account names, task ID, and a freshly minted
per-run workspace when a selected server references `${FLEET_WORKSPACE}`. The
returned broker/catalog feeds the scheduled `Agent` and remote-overlay shadow
set, and cleanup uses a fresh bounded context after cancellation. Scope-open
errors fail the run closed. The local binder remains for transitional callers;
production startup does not inject the opener yet.

## Approval integration

Interactive approval staging and resolution no longer reach through the HTTP
engine contract for a concrete `mcp.Client`. They receive the common `MCPBroker`
plus its public catalog, resolve the full `mcp_<server>_<tool>` identity against
that catalog, and delegate the call. Email pre-validation stays on the same
server that authored the staged send tool, rather than using an ambiguous bare
tool lookup. A tool-level MCP error is recorded as a failed approval execution.

The shipped Manager adapter still wraps its local client. This change is the
transport seam needed for process isolation; it does not yet preserve a scoped
account selection across a long-lived approval card. That lifecycle and the
production broker injection remain part of #167.

## Connector environment inventory

`clientconfig.Bundle` captures connector environment names from the raw
`mcp_servers` and `http_tools` sections before manifest interpolation. This
matters when a credential was exported before startup: interpolation replaces
`${VAR}` with its value in the parsed bundle, but the parent must still know the
original variable name after spawning the broker child. Only names are retained.

The inventory deliberately excludes parent-owned provider keys and webhook
signing secrets. For stdio connectors it also recognizes every
`<env-key>_<account>` variant that `ApplyClientSuffix` may read, not merely the
smaller `account_vars` discovery list. The production parent does not scrub
these keys yet; this is the complete, testable input for that switch-on step.
