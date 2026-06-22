// Package acpingress makes fleet an ACP AGENT over stdio: the INVERSE of
// internal/acpruntime, which makes fleet an ACP CLIENT driving sandboxed agents.
//
// Topology (Plan v4, P-ACP-3 — ingress):
//
//	editor (Zed / Neovim / any ACP host)
//	    │  spawns `fleet acp`, speaks ACP over stdio (JSON-RPC line-delimited)
//	    ▼
//	IngressAgent (acp.Agent)  ── north face: prompt in, session/update + request_permission out
//	    │  drives the SAME governed interactive turn the web path runs
//	    ▼
//	agent.Manager.RunTurn  ──► agentcore.Run (policy + sandbox + MCP + notes + audit + ceilings)
//	    │  underneath, the configured runtime FLAVOR (native-inprocess dev /
//	    ▼  native-acp prod sandbox via acpruntime.ClientRuntime) — inherited for free
//	fleet's existing governed, sandboxed pipeline
//
// THE CARDINAL RULE: ingress is a new INPUT/OUTPUT adapter on the EXISTING
// governed turn — NOT a second, weaker governance path. An ingress turn runs
// with the SAME policy, the SAME rootless-Podman sandbox, the SAME host-side
// MCP credential brokering, and the SAME notes/audit/cost-ceilings as the web
// interactive path, because it drives the SAME entrypoint (agent.Manager.RunTurn
// — the concrete httpapi.turnEngine). Only three things differ, and they are
// exactly the I/O surfaces:
//
//   - InputSource: the prompt arrives as an ACP PromptRequest (+ the conversation's
//     persisted history) instead of an HTTP/SSE request body;
//   - Observer: the run's streamed text / tool-call events go OUT as ACP
//     session/update notifications instead of SSE frames (see sink.go);
//   - Approval surface: a staged critical-tool / lockdown approval is routed to
//     the human via an OUTBOUND ACP session/request_permission instead of the web
//     approval card (see approver.go). Default-DENY on timeout / cancel /
//     no-options / a deny selection.
//
// Deliberate governance choices (documented, not gaps):
//
//   - Host-advertised mcpServers in NewSession are IGNORED. fleet brokers its OWN
//     client-config MCP catalog host-side (credentials never leave the host), so
//     accepting an editor's MCP endpoints would create an un-governed credential
//     path. Documented in docs/USING-AGENTS.md.
//   - Trust model: launching `fleet acp` runs as the box user = the same trust as
//     running fleet. This is LOCAL-PROCESS trust, distinct from the web path's
//     signed-key auth. Ingress sessions bind to a configured operator/service
//     Principal so audit attribution is correct. Remote ingress is out of scope.
//
// The package depends on narrow interfaces (TurnEngine / ConversationStore /
// StagedToolRunner — see seams.go) so it never imports the concrete *agent.Manager
// or *store.Store directly; cmd/fleet wires the real implementations, and tests
// inject fakes that drive a real governed turn against a fake LLM.
package acpingress
