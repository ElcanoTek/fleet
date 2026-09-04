# HTTP API versioning (#321)

Fleet's HTTP API is served under a **`/v1`** prefix — the stability-guaranteed
surface external consumers should build on. This document defines the contract.

## The `/v1` prefix

Every orchestrator and chat-server route is reachable two ways:

- **`/v1/<path>`** — the versioned surface. Responses carry an
  **`X-Fleet-API-Version: 1`** header. Prefer this.
- **`/<path>`** (bare, legacy) — the same handler, kept working for backward
  compatibility. Responses carry a **`Deprecation: true`** header and a
  **`Link: </v1/<path>>; rel="successor-version"`** header pointing at the
  versioned equivalent. Migrate off these.

A single wrapper (`internal/apiversion`) applies the prefix: `/v1/<path>` is
stripped and served by the identical handler at `<path>`. No route is registered
twice, so the OpenAPI route-parity test still walks the bare router; the spec's
`servers` block documents `/v1` as the primary base.

Health probes (`/healthz`, `/health`, `/readyz`) and version discovery
(`/api-info`) are **unversioned forever** — never prefixed-only, never
deprecation-tagged.

## Version discovery — `GET /api-info`

Unauthenticated (same posture as `/health`), reachable at `/api-info` and
`/v1/api-info`:

```json
{
  "api_version": "1",
  "fleet_version": "2026.09.04.2",
  "supported_versions": ["1"],
  "deprecated_versions": [],
  "schema_url": "https://github.com/ElcanoTek/fleet/blob/main/docs/openapi.yaml"
}
```

`api_version` is the **API** major version — it increments only on a breaking
change (below), independently of `fleet_version`. The two are unrelated on
purpose: `fleet_version` is the **date-based** identity of the running build
(`YYYY.MM.DD.N`, or `<release>+<n>.g<sha>` for a box tracking `main` between
releases — see [`VERSIONING.md`](VERSIONING.md)), which carries no compatibility
promise at all. `api_version` is the compatibility contract. A client can assert
`api_version` (or the `X-Fleet-API-Version` header) at startup to confirm
compatibility; it should treat `fleet_version` as an opaque label for support
tickets, never parse it, and never gate behaviour on it.

## What is a breaking change

| Change | Classification | Action |
|--------|----------------|--------|
| Remove a field from a response | **Breaking** | new major (`/v2`) |
| Change a field's type / meaning | **Breaking** | new major (`/v2`) |
| Remove or rename an endpoint | **Breaking** | new major (`/v2`) |
| Make an optional request field required | **Breaking** | new major (`/v2`) |
| Change the auth scheme | **Breaking** | new major (`/v2`) |
| Add an optional request field | Non-breaking | stays on `/v1` |
| Add a new endpoint | Non-breaking | stays on `/v1` |
| Add a field to a response | Non-breaking | stays on `/v1` |

Clients MUST tolerate unknown response fields (a non-breaking addition must not
break them).

## Reaching the API through the TLS front

On the single-box install the Go listeners bind loopback (chat
`127.0.0.1:8080`, orchestrator `127.0.0.1:8000`) and Caddy is the only thing
on `:443`. The fleet-managed Caddyfile (`scripts/lib/caddyfile.sh`, reference
copy in `deploy/Caddyfile`) routes the public API past the Next.js web tier:

| Public path | Upstream | Why it is exposed |
|---|---|---|
| `/v1`, `/v1/*` | orchestrator | the versioned surface — every `openapi.yaml` route |
| `/api-info` | orchestrator | version discovery, unversioned forever |
| `/.well-known/agent-card.json`, `/a2a`, `/a2a/*` | orchestrator | A2A discovery + JSON-RPC ([A2A.md](A2A.md)) |
| `/triggers/*` | orchestrator | webhook + email task triggers ([EVENT-TRIGGERS.md](EVENT-TRIGGERS.md)) |
| `/webhooks/*` | chat | GitHub/Slack-signed inbound webhooks ([WEBHOOKS.md](WEBHOOKS.md)) |
| everything else | Next.js web tier | pages, `/_next/*`, Next's own `/api/*` proxy |

So `https://<domain>/v1/tasks` with `X-API-Key` is the supported way in;
the **legacy bare paths are not routed publicly** (`https://<domain>/tasks`
reaches the web app, not the orchestrator) — they are reachable only from the
box, which is one more reason to migrate to `/v1`. Both backend routes strip
the Next-proxy header-trust headers (`X-User-Email`, `X-User-Session-Epoch`,
`X-Orchestrator-Server-Token` / `X-Chat-Server-Token`), so `X-API-Key` and the
webhook signatures are the only public authentication paths
([ADR-0053](adr/0053-public-api-through-the-tls-front.md)).

If `curl https://<domain>/api-info` returns the web app's HTML 404 instead of
the JSON above, the box is running a Caddyfile that predates these routes:
`sudo fleet doctor` rewrites a fleet-managed one (and probes `/api-info`
through Caddy afterwards); `sudo fleet update --adopt-units` does the same.

## Not yet implemented (honest scope)

- **A `Sunset` date** on the legacy bare paths: the issue's plan is `Sunset:
  <GA + 6 months>` and an eventual `410 Gone`. There is no GA date yet, so the
  legacy paths carry `Deprecation`/`Link` (the migration signal) but no `Sunset`
  and are not yet removed. Set the sunset window at GA.
- The bare paths still serve fully (the web UI + `fleet-admin` use them today);
  their migration to `/v1` is the follow-on the deprecation window enables.
