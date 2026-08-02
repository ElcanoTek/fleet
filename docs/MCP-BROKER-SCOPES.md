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

## Why this is separate

Scheduled runs can select different credential accounts and carry different task
identity/workspace values. A single process-wide MCP client cannot represent
those choices safely. The scoped protocol is the prerequisite that lets the
broker process construct and own a distinct client for each run while the agent
loop sees only the existing governed call seam.

## Deliberately deferred

The protocol and child backend are implemented, but production startup and run
construction do **not** yet use them. `agent.New` still builds the credentialed
client in the main fleet process, and per-user remote MCP credentials are not
covered by these scopes. Issue #167 remains open until the production interactive
and scheduled paths use the subprocess, parent credential material is scrubbed,
and the remote-MCP credential path is moved behind an equivalent process
boundary.

No authorization boundary is added here. Account allowlists, task policy, tool
approval, and audit remain responsibilities of the existing governed runtime;
the scope transports the already-authorized selection and cannot widen it.

## Driver integration

The interactive driver accepts an injected `MCPBroker` and public `MCPCatalog`
on `TurnConfig` and threads them into the same `agentcore.Run` call used by the
local-client path. Its per-user remote-MCP overlay composes with either base: a
remote server name routes to the short-lived user client, while every bundle
server routes to the injected broker. This is a wiring prerequisite only;
`Manager` does not yet supply these fields in production.

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
