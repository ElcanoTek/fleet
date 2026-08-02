# ADR-0040: Child-owned per-user remote MCP runtime

- **Status:** Accepted
- **Date:** 2026-08-02
- **Deciders:** fleet maintainers
- **Supersedes:** ADR-0009's in-process runtime-client decision

## Context

ADR-0009 introduced per-user hosted MCP connections and deliberately built each
run's bearer-authenticated HTTP client in the main Fleet process. The bundle MCP
catalog has since moved behind the `fleet mcp-broker` process boundary required
by ADR-0003, but leaving the dynamic per-user path in-process meant agent-run
code still shared an address space with decrypted access/API keys, refresh
tokens, and remote MCP transports while a turn was active.

The remote catalog is runtime data rather than bundle data, and interactive and
scheduled runs have different selection semantics. The boundary therefore must
carry a user identity and public server-name filters without carrying connection
rows, endpoint URLs, or credential values, while preserving the one governed
`agentcore.Run` path.

## Decision

Production interactive and scheduled runs open per-user remote MCP scopes in the
credential-owning child. The parent sends only the user's email, public enabled
and shadowed server names, and an explicit filter bit. The child opens its own
bounded chat-store pool, installs the at-rest cipher, looks up and decrypts or
refreshes the selected credentials, constructs the SSRF-guarded HTTP MCP
clients, and owns them through scope close. The parent receives only an opaque
scope ID, public tool metadata, and public names of skipped servers; calls cross
the existing `agentcore.MCPBroker` seam.

The main process does not inject `RemoteMCPResolver` into either run driver.
It retains `remotemcp.Service` only for explicit HTTP control-plane operations:
OAuth authorization/callback and runtime connector management, including API-key
intake and probes. Those operations necessarily handle user-supplied credentials
before encrypted storage, remain host-side, and are not model-callable. The main
store cipher also protects provider and notification secrets, so possession of
the at-rest key by the parent remains an explicit control-plane condition rather
than an address-space isolation claim against arbitrary parent compromise.

The child inherits the credential environment once at boot. Its remote-store
pool is capped at eight open and two idle connections (or lower configured
limits), repeats the chat/scheduler database-separation check, and closes its
pool on shutdown. A whole-scope resolver failure crosses the wire as one fixed,
value-free error; individual token/handshake failures return only skipped public
server names. Scope cleanup uses a fresh bounded context after run cancellation.

## Enforcement

- `cmd/fleet/main.go` injects only `OpenRemoteMCPOverlay` into both production
  drivers; the parent-side `remotemcp.Service` is wired only to HTTP/catalog
  control-plane handlers.
- `cmd/fleet/mcp_broker_runtime_test.go` locks the public-only selector,
  deterministic name ordering, opaque scoped routing, skipped-name propagation,
  and cleanup adapter.
- `cmd/fleet/mcp_broker_test.go` covers child-owned store/cipher construction,
  database separation, interactive-empty versus scheduled-all selection,
  credential-free error behavior, and remote-client reaping.
- The shared manager/scheduled overlay tests preserve selection, shadowing,
  validation, cancellation-independent close, and the unchanged governed call
  seam.

## Consequences

- Normal agent runs no longer decrypt per-user MCP credentials or instantiate
  their remote transports in the main process.
- The parent/child protocol exposes user identity and public connector names.
  They are authorization/routing metadata, not secrets, but still require the
  same host IPC protection as other scope metadata.
- The child adds a small independent database pool and per-run scope lifecycle.
- Explicit credential enrollment and OAuth callbacks remain parent-side
  control-plane exceptions. This ADR does not claim that a total compromise of
  the main Fleet process cannot access its database or encryption key.

## Alternatives considered

- **Keep the ADR-0009 in-process overlay.** Rejected because it leaves live
  run-time connector credentials in the agent loop's address space.
- **Send a bearer or connection URL over the broker protocol.** Rejected because
  the parent would still acquire the secret and the wire would become a
  credential transport.
- **Move every OAuth/connectors HTTP endpoint into the child in this change.**
  Deferred: browser redirects and user credential intake are explicit host
  control-plane operations, not agent-run data-plane calls. Moving them requires
  a separate authenticated control protocol and does not change where model-
  initiated MCP calls execute.
