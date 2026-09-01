# Machine clients: reaching the fleet API from another system

The 20-minute version of what an integrator (an intake app, a CI job, a
sibling service) needs to call fleet, written down because every piece of it
used to be discoverable only by reading `deploy/Caddyfile`, `web/src/proxy.ts`
and `internal/sched/handlers/middleware.go`. The task API itself is toured in
[`BUILDING-ON-FLEET.md`](BUILDING-ON-FLEET.md) and specified in
[`openapi.yaml`](openapi.yaml); this page is about **getting a request to it
at all**.

## 1. Where the API lives

fleet's Go listeners bind loopback: chat on `127.0.0.1:8080`, the orchestrator
(the task API) on `127.0.0.1:8000`. On the single-box install nothing else is
on `:443` but Caddy, and Caddy routes by path
([ADR-0053](adr/0053-public-api-through-the-tls-front.md)):

| `https://<domain>` path | goes to | note |
|---|---|---|
| `/v1`, `/v1/*` | orchestrator | **the API base — use this** |
| `/api-info` | orchestrator | unauthenticated version discovery |
| `/.well-known/agent-card.json`, `/a2a`, `/a2a/*` | orchestrator | A2A ([A2A.md](A2A.md)) |
| `/triggers/*` | orchestrator | webhook + email task triggers |
| `/webhooks/*` | chat | GitHub/Slack-signed chat webhooks |
| everything else | Next.js web tier | pages + Next's own `/api/*` proxy |

Two consequences integrators hit first:

- **Bare paths are not the API.** `https://<domain>/tasks` reaches the web
  app, which redirects a cookieless request to `/login` (a `307`); the same
  request under `/v1/tasks` reaches the orchestrator. Every route in
  `openapi.yaml` is relative to `/v1`.
- **A box provisioned before the API routes shipped has none of them** —
  `/v1/*` also lands on the web app and 307s/404s. `sudo fleet doctor`
  detects that Caddyfile, rewrites it (backup kept) and then proves
  `https://<domain>/api-info` answers through Caddy; `sudo fleet update` offers
  the same rewrite. Nothing to hand-edit.

Quick reachability check, no credentials needed:

```sh
curl -sS https://fleet.example.com/api-info
# {"api_version":"1","fleet_version":"…","supported_versions":["1"],…}
# HTML or a redirect here = the proxy is not routing the API (run: sudo fleet doctor)
```

## 2. Mint a key — on the box, into the service's store

```sh
sudo fleet sched apikey create manifest-dev --type task
# key store: /var/lib/fleet/data/api_keys.json (the fleet.service store …)
# created API key manifest-dev (id=key_… type=task)
# secret (shown once): fleet_task_…
# format: fleet_task_<base58> — send it as the X-API-Key header; …
```

What to know:

- **Key shapes.** Typed keys are `fleet_<type>_<base58>`: `fleet_task_…`
  creates and reads its own tasks, `fleet_readonly_…` only reads,
  `fleet_webhook_…` only fires its named triggers, `fleet_admin_…` is
  everything. Legacy `--role` keys are `sk-<base64>`. `--help` and the create
  output both say this.
- **The store.** The service reads `api_keys.json` under **its** data dir —
  `FLEET_DATA_DIR`/`DATA_DIR` from `/etc/fleet/fleet.env`, else `./data`,
  relative to the unit's `WorkingDirectory` (`/var/lib/fleet`). The CLI now
  derives the same path, prints `key store: …` on every run, and **warns**
  when `FLEET_DATA_DIR`/`DATA_DIR` in your shell points it somewhere else.
  A key minted into any other file is invisible to the service, and the
  caller sees a `401` indistinguishable from a typo.
- **Ownership.** A root-run mint re-owned `api_keys.json` to root inside the
  `fleet` user's directory, so the service could neither read the new key nor
  save its own changes. The CLI now hands the store files back to the
  directory's owner after every write (`chowned … to uid …` when it had to).
- The service picks a new key up **without a restart** (it re-reads the file
  on a lookup miss when the mtime moved).

## 3. Authenticate

Send the key as `X-API-Key`. That is the only public authentication path:
the proxy deletes the Next-proxy header-trust headers (`X-User-Email`,
`X-User-Session-Epoch`, the shared token headers) on every request it
forwards, so a cookie- or header-impersonation path does not exist from
outside the box.

```sh
FLEET=https://fleet.example.com/v1
KEY=fleet_task_…
curl -sS -X POST "$FLEET/tasks/estimate" -H "X-API-Key: $KEY" \
  -H "Content-Type: application/json" -d '{"prompt":"ping","model":"anthropic/claude-opus-4-8"}'
```

`POST /v1/tasks/estimate` is the recommended **connection test**: it runs the
same auth gate as a create, needs the same key scope, costs nothing and
creates nothing — a `200` proves reachability + key together; a `401` is a
key the service does not have (wrong store, wrong prefix, revoked); a `403`
is a valid key of the wrong type for the route.

## 4. What a client should build on

- Base URL `https://<domain>/v1`; assert `X-Fleet-API-Version: 1` or
  `GET /api-info` at startup ([api-versioning.md](api-versioning.md)).
- Tasks: `POST /v1/tasks`, `GET /v1/tasks/{id}`, `/stream`, `/output` —
  [`BUILDING-ON-FLEET.md`](BUILDING-ON-FLEET.md).
- Outcome callbacks: task notification webhooks ([NOTIFICATIONS.md](NOTIFICATIONS.md),
  signing in [WEBHOOK-SIGNING.md](WEBHOOK-SIGNING.md)).
- Firing fleet **from** an external event: `POST /v1/triggers/{slug}` with a
  `fleet_webhook_…` key ([EVENT-TRIGGERS.md](EVENT-TRIGGERS.md)).

## 5. Operator knobs that go with a machine client

- `MANIFEST_CALLBACK_TOKEN`-style secrets and any other env var: 
  `printf '%s' "$V" | sudo fleet config set-env KEY` — one line, deduped,
  file kept `0600` with its owner unchanged ([ENV-CLI.md](ENV-CLI.md)).
  Hand-editing is how a duplicate line took a deployment down: the server
  reads the **last** occurrence.
- `FLEET_TRUSTED_PROXIES=127.0.0.1,::1` so per-client rate limits and IP
  lists see the real caller behind Caddy ([DEPLOYMENT.md](DEPLOYMENT.md)).
- `sudo fleet status` probes the sandbox image in the **service user's**
  podman store (root's store is a different namespace; the old root-run
  report said "missing" for an image doctor had just verified).
