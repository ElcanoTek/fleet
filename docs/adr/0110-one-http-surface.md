# ADR-0110: One HTTP surface — one chi tree on the public listener

- **Status:** Accepted
- **Date:** 2026-11-08
- **Deciders:** fleet maintainers
- **Issue:** #2050 ("One HTTP surface"), phase 3
- **Amends:** [ADR-0053](0053-public-api-through-the-tls-front.md) (where the
  public API is served), [ADR-0091](0091-go-serves-the-browser-tier.md) (what
  the web listener serves)
- **Builds on:** [ADR-0108](0108-typed-principal.md),
  [ADR-0109](0109-generated-http-contracts.md) §2

## Context

`fleet serve` ran three listeners with three handlers: the browser tier on
`FLEET_WEB_ADDR` (`127.0.0.1:3000`), chat on `FLEET_SERVER_ADDR`
(`127.0.0.1:8080`) and the orchestrator on `FLEET_ORCHESTRATOR_ADDR`
(`127.0.0.1:8000`). A front proxy had to know which paths went where: the box
Caddyfile sends `/v1`, `/api-info`, the agent card, `/a2a` and `/triggers/*`
to `:8000`, `/webhooks/*` to `:8080`, and everything else to `:3000`.

The browser reached the Operations Center through a table of about 45 rows in
`internal/webui/routes.go`, each translating `/api/orchestrator/<x>` to an
orchestrator path and calling the router in-process with a synthesised
request. Every orchestrator route the browser needed was a row someone had to
add, and a row could point at a route that did not exist.

## Decision

1. **One chi tree serves every family on the public listener**
   (`cmd/fleet/router.go`, `buildPublicRouter`):

   | Path | Family | Served by |
   |---|---|---|
   | `/v1`, `/v1/*` | orchestrator | the orchestrator listener's handler |
   | `/api-info`, `/.well-known/agent-card.json`, `/a2a`, `/a2a/*`, `/triggers/*` | orchestrator | the same |
   | `/webhooks/*` | chat | the chat listener's handler |
   | `/api/orchestrator`, `/api/orchestrator/*` | orchestrator (browser alias) | the gateway, which serves the same router behind the browser boundary |
   | everything else | webui | the gateway |

   The API paths answer on the public listener **byte for byte** as on their
   own listener, because they are served by the very same handler (health
   probes, `apiversion.Router`, the router and its chain). External clients
   keep `/v1` with `X-API-Key`; nothing under it changes shape.

2. **A family keeps its whole chain; the tree adds none.** The tree has no
   middleware of its own. A mounted family routes the request afresh on its
   own path (the parent's chi routing state is hidden), so its middleware
   chain, its metric labels (`RoutePattern`), its path-based exemptions (the
   orchestrator's CSRF exemption for `/a2a` and `/triggers`) and its handlers
   see exactly what they see on their own listener. No read or write timeout
   is added: SSE streams and long polls behave as on every listener.

3. **`/api/orchestrator/*` is the orchestrator router, not a table.** The
   gateway serves every route the router registers under the prefix
   (`internal/webui/orchestrator_mount.go`), after the browser boundary the
   rows had, in the same order: the session gate, the route (a path the
   router does not register is the gateway's JSON 404/405), CSRF on a
   mutating verb, the credential (a bearer, else the session as a typed
   principal), the backend contract (fresh headers, the loopback peer, a body
   only on a mutating verb), and the forwarded-header response. The request
   body streams to the router; no path or query is re-encoded. The two
   non-prefix rows (`/config` → `/api/config`, `/me` → `/api/me`) are
   explicit routes, and the query allow-lists are an explicit per-route policy
   (`orchQueryPolicy`) held to the real router by a test. A route the browser
   never had a row for forwards its query verbatim, as on `/v1`. The orchestrator rows of
   `routes.go` are deleted.

4. **The loopback listeners stay, serving the same handlers.** The box
   Caddyfile routes the API paths to `:8000` and `:8080`; `fleet chat`,
   `fleet acp`, fleet-bench, the admin CLI and the release health probe dial
   them; Helm exposes all three ports. Retiring them is a front-proxy and
   client change, not a router change, so it is not made here and no setting
   is retired: `FLEET_SERVER_ADDR` and `FLEET_ORCHESTRATOR_ADDR` are still
   read. With this decision in place, a front proxy *may* send the whole
   origin to the public listener; the managed Caddyfile is not changed, so no
   box needs an edit.

## Invariant

The public listener serves browser and API traffic from one router. Each
family's public paths answer there exactly as on the family's own listener,
and each family's middleware chain is its whole chain on every mount.

Enforced by:

- `TestOneSurfaceParity` (`cmd/fleet`): every browser row the gateway
  answered before this change (`testdata/http-surface-before.txt`, captured
  from the pre-change tree) gives the same upstream request, status, headers
  and auth requirement through the new tree; the `/v1` route set is
  unchanged.
- `TestBrowserAliasMountsTheV1RouteSet`: `/api/orchestrator` registers
  exactly the `/v1` route set, which is the router's own.
- `TestPublicRouterAnswersV1LikeTheOrchestratorListener`,
  `TestPublicRouterFamilies`, `TestPublicRouterStreamsUnbuffered`,
  `TestOrchestratorMiddlewareChainOrder`, and in `internal/webui`
  `TestOrchestratorMountBoundaryOrder`.
- `TestOpenAPIGenerated` walks the public tree with `Family.Prefix = "/v1"`.

## Consequences

- The browser can reach every Operations Center route under
  `/api/orchestrator`, not only the 45 that had rows. Each route enforces its
  own auth and permission for a session principal exactly as it did for the
  rows (ADR-0108), and CSRF still applies to every mutating verb. A new
  orchestrator route needs no gateway change.
- The API paths are now also served on the public port. On a box behind the
  managed Caddyfile nothing changes (Caddy matches them first and sends them
  to `:8000`/`:8080`). A deployment that forwards the whole public port, such
  as an ingress in front of the Helm Service's `web` port, now reaches `/v1`
  and `/webhooks` there too, with the same `X-API-Key` and signature auth they
  have on their own ports. Nothing else becomes reachable: the orchestrator's
  and chat's bare paths stay on loopback.
- The chat family is not walked yet. Its routes are a `net/http.ServeMux` of
  prefix patterns that dispatch sub-paths in handlers, and its browser routes
  are per-route translations with their own body rules and response shapes,
  not a prefix alias. They stay gateway rows. Moving chat's registration to
  chi, and its catalog (#2050 phase 4 step 2), is follow-up work.
- Three gateway-owned Operations Center routes still call the router with a
  synthesised request because they shape the response themselves: the live
  log stream (SSE headers), the workspace file download (file hardening) and
  "discuss this run" (two reads and a chat create).
