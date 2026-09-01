# ADR-0053: The public HTTP API is served through the TLS front, with the header-trust channel stripped

- **Status:** Accepted
- **Date:** 2026-09-01
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0004](0004-single-box-vm-native-deployment.md) (the
  single-box Caddy front stands; its "the Next.js app is the only public
  entrypoint" wording is narrowed to "the only public entrypoint for
  browsers")

## Context

fleet has documented a public HTTP API for a long time: `docs/openapi.yaml`
names `/v1` as the stability-guaranteed base, `docs/api-versioning.md` defines
the contract, `docs/WEBHOOKS.md` tells operators to point GitHub and Slack at
`https://your-fleet/webhooks/…`, `docs/A2A.md` publishes the agent card at the
spec-fixed `/.well-known/agent-card.json`, and the coupling doctrine in
`AGENTS.md` says intake apps integrate "by calling fleet's API".

None of that was reachable on a box provisioned by `scripts/bootstrap.sh
--enable-web --domain`. The Caddyfile it wrote — and the `deploy/Caddyfile`
reference — had exactly one upstream, the Next.js web tier on `:3000`. Next has
no `/v1`, `/api-info`, `/a2a`, `/triggers` or `/webhooks` routes, so every
documented API URL answered with the web app's 404 page while every unit was
green. The Go listeners (chat `127.0.0.1:8080`, orchestrator `127.0.0.1:8000`)
were reachable only from loopback, by design: the orchestrator trusts
`X-User-Email` when the shared `X-Orchestrator-Server-Token` matches (the
Next-proxy impersonation channel, #157), and chat does the same with
`X-Chat-Server-Token`. "Loopback only" was how that channel stayed private.

Two things were therefore true at once: the API had to become reachable from
the internet, and the impersonation channel had to stay reachable from
loopback only.

## Decision

1. **Caddy routes the public API to the Go listeners, and nothing else of
   theirs.** The orchestrator's versioned surface (`/v1`, `/v1/*`) and the
   paths fleet fixes as unversioned-forever (`/api-info`,
   `/.well-known/agent-card.json`, `/a2a`, `/a2a/*`, `/triggers/*`) go to the
   orchestrator listener; chat's signed inbound webhooks (`/webhooks/*`) go
   to the chat listener; everything else stays with the Next.js web tier.
   The orchestrator's legacy bare paths (`/tasks`, `/keys`, …) are **not**
   exposed — they remain behind Next's `/api/*` proxy — so the public API is
   exactly the versioned one the docs promise.
2. **The header-trust channel is stripped at the proxy.** Both backend routes
   delete `X-User-Email` and `X-User-Session-Epoch`, plus the respective shared
   token header (`X-Orchestrator-Server-Token` / `X-Chat-Server-Token`), before
   forwarding. A request that arrives from the internet therefore cannot
   present the impersonation headers at all, whatever it knows; only requests
   originating on the box (Next's server-side proxy) can. Exposing the API adds
   exactly one public authentication path — `X-API-Key` (typed keys or the
   bootstrap admin key), plus per-trigger webhook signatures — and no
   header-trust surface. The listeners themselves still bind loopback.
3. **One renderer, three consumers.** The layout lives once, in
   `scripts/lib/caddyfile.sh` (`render_fleet_caddyfile`). `bootstrap.sh`
   writes the live file from it, `doctor.sh` diffs the installed file against
   it and rewrites a drifted **fleet-managed** file (marker present; backup
   kept; `caddy validate`; `systemctl reload caddy`), and `update.sh` offers
   the same rewrite under its unit-adoption consent rule (`--adopt-units` or an
   interactive yes). `deploy/Caddyfile` remains the annotated reference and a
   test pins its functional body to the renderer's output. A Caddyfile fleet
   did not write is never rewritten by any of them.
4. **Doctor proves routing, not just process health.** Beyond `/healthz` and
   `/readyz` on loopback, `fleet doctor` fetches `https://<domain>/api-info`
   through the local Caddy (`--resolve` pinned to `127.0.0.1`) and fails when
   the orchestrator's JSON does not come back. The in-process
   `GET /admin/doctor` report gains a structural version of the same check.

## Consequences

- API clients, A2A peers, GitHub/Slack webhooks and the email/webhook task
  triggers work on a bootstrapped box at the documented URLs. Already-
  provisioned boxes pick the routing up from `sudo fleet doctor` (repairs) or
  `sudo fleet update` (offers), or from re-running bootstrap, which now
  reloads a running Caddy instead of only enabling it.
- Behind the proxy, the backends see Caddy's loopback address as the peer.
  Operators who use `FLEET_IP_ALLOWLIST`/`FLEET_IP_DENYLIST` or care about
  per-client rate limiting should set `FLEET_TRUSTED_PROXIES=127.0.0.1,::1`
  so the real client IP is read from `X-Forwarded-For`; the default (never
  consult the header) remains the safe one and is unchanged.
- The orchestrator's admin surface (`/v1/keys`, `/v1/users`, …) is reachable
  from the internet **when the caller holds an admin API key**. That is the
  ordinary posture for an API with admin keys, and it is the reason the
  bootstrap admin key is generated rather than defaulted; nothing about key
  handling changes here.
- The invariants in `AGENTS.md` are untouched: credentials stay host-side,
  the sandbox is unchanged, `agentcore.Run` remains the one governed loop.
  What changes is a deployment claim — "Next is the only public entrypoint"
  — which was already contradicted by the API docs; this ADR makes the
  deployment match them, with the one property that claim protected (a
  loopback-only impersonation channel) enforced explicitly instead.
