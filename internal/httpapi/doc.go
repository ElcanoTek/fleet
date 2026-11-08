// Package httpapi exposes chat-server's HTTP surface to the web tier.
//
// # Trust model
//
// chat-server never talks to browsers directly. Every handler reads the
// caller's identity from the request context's [principal.Principal]
// (#2050, ADR-0108), never from a header:
//
//   - the web tier (internal/webui) calls these handlers in-process and
//     attaches the principal itself — the session it verified, or its own
//     principal for the sign-in legs and the public brand assets;
//   - a request on the chat listener's own port carries none until the edge
//     (edge.go) resolves the credential it presented.
//
// [Server.membershipMiddleware] then resolves the account
// ([principal.Resolve]); the email it carries is the row-level scope of every
// SQL query.
//
// The only unauthenticated endpoint is GET /healthz.
//
// # Endpoints
//
//   - GET    /healthz                           — liveness probe
//   - GET    /personas                          — list persona YAMLs
//   - GET    /conversations                     — list user's conversations
//   - POST   /conversations                     — create empty conversation (optional seed message, docs/DISCUSS-RUN.md)
//   - GET    /conversations/{id}                — full history replay
//   - DELETE /conversations/{id}                — delete (+ cascade messages)
//   - POST   /conversations/{id}/pin            — { pinned: bool }
//   - POST   /chat                              — start a turn, stream SSE
//
// # SSE framing
//
// [Server.postChat] creates an [sseSink] wrapping the response writer and
// passes it to [agent.Manager.RunTurn]. Every streaming callback from
// fantasy becomes one SSE event: reasoning.{start,delta,end}, text.delta,
// tool.call, tool.result, turn.{completed,error}. See sse.go for the framing.
package httpapi
