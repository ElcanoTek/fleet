// Package mcp is a minimal Model-Context-Protocol client.
//
// It supports two transports:
//
//   - stdio — spawn a Python/Node/etc. subprocess and speak JSON-RPC over
//     its stdin/stdout. Used for the email and SSP/DSP MCP servers bundled
//     with fleet (see mcp/*.py). Credentials are injected host-side via the
//     subprocess environment in [NewStdioTransport]; they never reach the
//     sandbox container and never appear on argv.
//   - http  — JSON-RPC over POST to a remote MCP endpoint, including the
//     SSE (text/event-stream) response form used by some hosted servers.
//
// # Lifecycle
//
// One [Client] is constructed at boot. Enabled servers are registered via
// [Client.AddStdioServer] or [Client.AddHTTPServerWithHeaders]; the client
// performs the MCP initialize + tools/list handshake synchronously, so
// [Client.GetAllTools] is safe to call immediately after. Call [Client.Close]
// on shutdown to reap subprocesses.
//
// # Tool naming and dispatch
//
// Tools are advertised to the agent layer as mcp_<server>_<tool> to avoid
// collisions across servers with overlapping tool names (sendgrid and
// mailbux both export send_email, for example). The wrapping happens in the
// agent package, not here. Because bare names overlap, callers that know
// their target should dispatch with [Client.CallToolOn] or
// [Client.CallToolPrefixed] rather than the bare-name [Client.CallTool].
//
// # Resilience
//
// A stdio subprocess that dies mid-session is restarted transparently on the
// next call. The retry is replayed only when the request provably never
// reached the server (a write-side failure, or a transport poisoned by an
// earlier cancelled call); a read-side death — where the server may already
// have executed the call — surfaces an explicit "outcome UNKNOWN" error so a
// non-idempotent action (send_email, deal-create) is never silently
// double-executed.
package mcp
