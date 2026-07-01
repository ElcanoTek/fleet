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
  "fleet_version": "1.2.0",
  "supported_versions": ["1"],
  "deprecated_versions": [],
  "schema_url": "https://github.com/ElcanoTek/fleet/blob/main/docs/openapi.yaml"
}
```

`api_version` is the **API** major version — it increments only on a breaking
change (below), independently of the `fleet_version` binary semver. A client can
assert `api_version` (or the `X-Fleet-API-Version` header) at startup to confirm
compatibility.

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

## Not yet implemented (honest scope)

- **A `Sunset` date** on the legacy bare paths: the issue's plan is `Sunset:
  <GA + 6 months>` and an eventual `410 Gone`. There is no GA date yet, so the
  legacy paths carry `Deprecation`/`Link` (the migration signal) but no `Sunset`
  and are not yet removed. Set the sunset window at GA.
- The bare paths still serve fully (the web UI + `fleet-admin` use them today);
  their migration to `/v1` is the follow-on the deprecation window enables.
