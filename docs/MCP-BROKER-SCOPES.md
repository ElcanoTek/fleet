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
