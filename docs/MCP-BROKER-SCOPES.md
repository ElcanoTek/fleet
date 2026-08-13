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

Scope-open also has an additive remote-overlay selector. A remote request carries
the user's email, public enabled and shadowed server names, and an explicit
`filterEnabled` bit; it never carries a connection row, endpoint URL, access or
refresh token, API key, or OAuth client secret. The bit preserves the semantic
difference between a scheduled run's “all connected servers” and an interactive
run with an empty enabled list. Remote and bundle scope fields cannot be mixed,
an empty user is rejected, and enabled names without the filter bit are rejected
before backend dispatch. A successful response may include public names for
selected servers that could not be connected, exposed through `Scope.Skipped()`
as a defensive copy alongside the defensive tool-catalog copy.

The `fleet mcp-broker` backend implements that interface. It resolves account
suffixes, identity-routing refusal, `${FLEET_TASK_ID}`, and
`${FLEET_WORKSPACE}` inside the child; owns one MCP client per opaque scope; and
serializes scope close against calls already admitted to that client. Closing
the broker after its protocol loop exits also reaps every scope a disconnected
parent left open. Disabled servers are absent from the shared spawn-definition
map, so a stale selection cannot launch one through either the broker or the
legacy scheduled binder.

When remote MCP is configured, the child also opens the chat store, installs the
same at-rest cipher, and constructs the `remotemcp.Service` there. That store has
an independent pool capped at eight open and two idle connections (or a lower
configured chat-pool limit), and the child repeats the chat/sched database
separation check before it can migrate. A remote scope resolves the user's own
and shared connections, decrypts or refreshes the selected credentials under the
existing row lock, and builds the SSRF-guarded HTTP MCP client entirely inside
the child. An explicit empty interactive filter still opens an empty lifecycle
scope without looking up a token; scheduled all-connected selection and
needs-reauth skipped-name reporting preserve the legacy behavior. The broker
owns each resulting client until scope close or peer disconnect, then closes its
remote DB pool during shutdown.

Scope-open resolver and per-server token/handshake failure values are
deliberately not logged by the overlay builder or returned through scope open.
A whole-scope resolver failure becomes the fixed `remote MCP scope unavailable`
error; recoverable per-server failures return only public skipped names.
All credential-owner operational failures follow the same rule: call,
discovery, scope-open, scope-close, and reload errors cross the pipe only as
stable value-free classes. A failed call also discards partial text and the
tool-error bit. Successful MCP tool-level errors remain model-visible output;
protocol validation remains precise because neither originates as credentialed
backend detail.

The protocol also supports credential-owner reload. The parent sends an empty
`reload` request — never resolved server definitions — and the child re-reads
its bundle against its boot credential snapshot, applies the same
minimum add/remove/restart diff as the in-process client, and returns only the
public summary, refreshed tool catalog, provisioned account-seat names, and the
enabled servers' gating/picker metadata.
Reload requests use the existing correlation, cancellation, and value-free
panic-containment machinery. Scope opening serializes with reload: an opening
scope receives a coherent old or new base catalog, while scopes already open
keep their original client until close. Inline `http_tools` remain boot-pinned,
matching the existing reload contract.

## Why this is separate

Scheduled runs can select different credential accounts and carry different task
identity/workspace values. A single process-wide MCP client cannot represent
those choices safely. The scoped protocol is the prerequisite that lets the
broker process construct and own a distinct client for each run while the agent
loop sees only the existing governed call seam.

## Production boundary

Production startup and both bundle run paths use these scopes. Per-user remote
hosted MCP is also activated through the transport-neutral opener: the parent
sends the child only identity and public selection names, and the child owns
token lookup/refresh and the short-lived remote client (ADR-0040). Explicit
OAuth/connectors HTTP control-plane operations remain parent-side; no agent run
driver receives their credential resolver.

No authorization boundary is added here. Account allowlists, task policy, tool
approval, and audit remain responsibilities of the existing governed runtime;
the scope transports the already-authorized selection and cannot widen it.

## Driver integration

The interactive driver accepts an injected `MCPBroker` and public `MCPCatalog`
on `TurnConfig` and threads them into the same `agentcore.Run` call used by the
local-client path. Its per-user remote-MCP overlay composes with either base: a
remote server name routes to the short-lived user client, while every bundle
server routes to the injected broker.

The remote overlay itself is now transport-neutral as well. It may own the
historical short-lived `mcp.Client`, or carry an injected `agentcore.MCPBroker`
with public catalog/routing metadata and a scope-close function. Manager and the
scheduled Runner each accept a remote overlay opener; when present it takes
precedence over the legacy token resolver. Both paths preserve interactive
opt-in filtering, scheduled all-connected semantics, bundle-name shadowing, and
skipped-server reporting. Broker-scope cleanup runs on a fresh five-second
context, so cancellation of the turn cannot strand the remote scope. This seam
rejects a broker overlay without an explicit close function, rather than
silently leaking a per-run scope. Production binds that seam to the child-owned
remote scope.

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
injects the broker, initial public catalog/account inventory, scope opener, and
reload adapter. Manager atomically swaps its enabled-server gates, public
catalog, provisioned account names, prompt roster, and picker metadata from one
self-describing child result. The scrubbed parent does not re-read connector
definitions on reload. A broker without that adapter fails an operator
reload explicitly instead of reporting a false success. Production broker
reload keeps credential-bearing connector configuration inside the child.

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
production injects the opener and a live public server inventory containing
only enabled names and whether each server uses `${FLEET_WORKSPACE}`.
Empty-selection expansion and workspace
minting then no longer read credential-bearing `cfg.MCPServers`, and a reload can
replace the inventory for the next run.

## Approval integration

Interactive approval staging and resolution no longer reach through the HTTP
engine contract for a concrete `mcp.Client`. They receive the common `MCPBroker`
plus its public catalog, resolve the full `mcp_<server>_<tool>` identity against
that catalog, and delegate the call. Email pre-validation stays on the same
server that authored the staged send tool, rather than using an ambiguous bare
tool lookup. A tool-level MCP error is recorded as a failed approval execution.

Production approval execution uses the unscoped child broker because an approval
card can outlive the turn scope that staged it. It therefore uses the default
bundle seat; preserving a scoped named-account selection across a long-lived
approval card remains deferred under #167.

## Connector environment inventory

`clientconfig.Bundle` captures connector environment names from the raw
`mcp_servers` and `http_tools` sections before manifest interpolation.
Connector env/header values keep their raw `${VAR}` text through `Load` and
resolve lazily against the live process env at catalog-build/spawn time, after
the `.env` file is applied — but the inventory cannot depend on when (or
whether) a given value resolves: other connector fields are still substituted
at load, a resolved value no longer carries its source name, and the parent
must know the exact names either way, to unset them after the broker child
boots and to register them as reload exclusions. Only names are retained.

The inventory deliberately excludes parent-owned provider keys and webhook
signing secrets. For stdio connectors it also recognizes every
`<env-key>_<account>` variant that `ApplyClientSuffix` may read, not merely the
smaller `account_vars` discovery list. Production unsets the resulting exact
keys only after the child passes liveness, tool discovery, and account discovery;
on any boot failure it leaves parent state intact and aborts startup. Resolved
connector maps/slices are overwritten before references are dropped, while a
separate public always-on view preserves the Connections UI.

The parent also registers the source and account-base names as permanent config
reload exclusions, preventing a later env-file reload from restoring either an
exact key or an uppercase account-suffixed variant. Connector names may not
overlap parent runtime settings, model-provider keys, webhook signing secrets,
or core process variables; startup refuses a name-only conflict report. Values
are never included in that report. Connector env values are boot-pinned in the
broker and require a process restart to rotate.

Because Go strings are immutable, this is reachability scrubbing rather than a
cryptographic heap-zeroization claim: the parent necessarily resolved connector
values during the boot handoff, then removes environment entries and overwrites
or drops all reachable credential-bearing connector definitions.
