// Package httpapi exposes chat-server's HTTP surface to the web tier.
//
// # Trust model
//
// chat-server never talks to browsers directly. Every request arrives from
// the web tier (internal/webui) carrying two headers:
//
//   - X-Chat-Server-Token: a shared secret set in both .env.local files.
//     Constant-time-compared in [Server.authMiddleware].
//   - X-User-Email: the authenticated user's email, pulled from the signed
//     session cookie by the web tier before proxying. Used for
//     row-level scoping on every SQL query.
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
