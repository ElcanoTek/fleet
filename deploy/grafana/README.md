# Fleet Grafana dashboard

A pre-built Grafana dashboard for the Prometheus metrics fleet exposes (issue
[#176]). Import [`fleet-dashboard.json`](fleet-dashboard.json), point it at a
Prometheus datasource that scrapes fleet, and you get instant observability —
no hand-authoring panels or guessing metric names.

## What it shows

The dashboard is built **only** from metrics fleet actually exports today. The
authoritative list lives in
[`internal/metrics/recorders.go`](../../internal/metrics/recorders.go):

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `fleet_http_requests_total` | counter | `route`, `method`, `status` | Served HTTP requests (route = chi route pattern). |
| `fleet_http_request_duration_seconds` | histogram | `route`, `method` | Request latency. Buckets: `.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10` s. |
| `fleet_active_agents` | gauge | `kind` (`interactive`/`scheduled`) | Agent turns in flight right now (sampled at scrape). |
| `fleet_cost_usd_total` | counter | `model` | Cumulative LLM spend in USD, per model. |
| `fleet_token_usage_total` | counter | `model`, `type` (`prompt`/`completion`/`cached`) | Cumulative token counts. |
| `fleet_sandbox_pool_size` | gauge | — | Warm Podman sandbox containers parked in the pool (sampled at scrape). |
| `fleet_turn_timeouts_total` | counter | `kind` (`interactive`/`scheduled`) | Turns that hit their wall-clock deadline. |
| `fleet_sched_runs_pruned_total` | counter | — | Scheduled task runs deleted by the retention sweep. |

That is the complete set. fleet's exporter is hand-rolled and **does not** ship
the default Go/process collectors, so there are no `go_*` or `process_*` series
to chart — and the dashboard does not pretend otherwise.

### Panel conventions

- Counter panels use `rate(...[$__rate_interval])` (scaled to per-hour where a
  human-friendly rate helps) — never raw counter values.
- The latency panels derive p50/p95/p99 with `histogram_quantile()` over
  `fleet_http_request_duration_seconds_bucket`. Because the largest explicit
  bucket is `10s`, a quantile whose true value exceeds 10s renders as `+Inf`;
  that is a signal, not a bug.
- Every panel reads from the `$datasource` and `$instance` template variables, so
  one dashboard serves many fleet boxes scraped by a shared Prometheus. The
  `$instance` selector is populated from `label_values(fleet_active_agents,
  instance)` (the `instance` label Prometheus attaches at scrape time).

## The scrape target: `/metrics`

fleet serves Prometheus text-format metrics from the **orchestrator** HTTP
backend at `GET /metrics`. By default the orchestrator binds **loopback**
`127.0.0.1:8000` (overridable via `FLEET_ORCHESTRATOR_ADDR`); see
[`deploy/Caddyfile`](../Caddyfile) for the topology.

> **The endpoint is authenticated.** `/metrics` is admin-API-key gated — cost and
> token data are not public. Your scraper must send the header
> `X-API-Key: <ADMIN_API_KEY>`, matching the `ADMIN_API_KEY` fleet runs with.
> A scrape without it gets `401 Unauthorized`.

### Prometheus scrape config

`prometheus.yml`:

```yaml
global:
  scrape_interval: 15s

scrape_configs:
  - job_name: fleet
    metrics_path: /metrics
    scheme: http
    # /metrics is admin-API-key gated; send the admin key as X-API-Key.
    authorization: # NOTE: Prometheus has no native X-API-Key field; see below.
    static_configs:
      - targets: ["127.0.0.1:8000"] # FLEET_ORCHESTRATOR_ADDR
```

Prometheus sends credentials via its `authorization` block (Bearer/Basic) or, in
Prometheus ≥ 2.46, an explicit `http_headers` block. fleet wants the key in the
`X-API-Key` header, so use `http_headers`:

```yaml
scrape_configs:
  - job_name: fleet
    metrics_path: /metrics
    static_configs:
      - targets: ["127.0.0.1:8000"]
    http_headers:
      X-API-Key:
        # Keep the key out of the config file — read it from a file on disk.
        files: [/etc/prometheus/fleet-admin-key]
```

If your Prometheus predates `http_headers`, scrape through a small reverse proxy
(e.g. the Caddy/Nginx fronting fleet) that injects `X-API-Key` on the way to
`/metrics`, and point Prometheus at the proxy.

## Importing the dashboard

### Via the Grafana UI

1. **Dashboards → New → Import**.
2. Upload [`fleet-dashboard.json`](fleet-dashboard.json) (or paste its contents).
3. When prompted, pick your Prometheus datasource for the `Datasource` variable.
4. Save. The dashboard UID is `fleet-core-v1`.

### Via file-based provisioning

Drop the JSON where Grafana's dashboard provider reads it and add a provider
config:

```yaml
# /etc/grafana/provisioning/dashboards/fleet.yml
apiVersion: 1
providers:
  - name: fleet
    type: file
    options:
      path: /var/lib/grafana/dashboards
```

then place `fleet-dashboard.json` in `/var/lib/grafana/dashboards/`.

## Local dev: Prometheus + Grafana with Docker Compose

A minimal stack to view the dashboard against a fleet running on the host. fleet
itself is **not** containerized here — Prometheus reaches it over
`host.docker.internal`.

`docker-compose.observability.yml`:

```yaml
services:
  prometheus:
    image: prom/prometheus:v3
    command: ["--config.file=/etc/prometheus/prometheus.yml"]
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml:ro
      - ./fleet-admin-key:/etc/prometheus/fleet-admin-key:ro
    extra_hosts:
      - "host.docker.internal:host-gateway" # Linux: reach the host
    ports:
      - "9090:9090"

  grafana:
    image: grafana/grafana:11
    environment:
      GF_AUTH_ANONYMOUS_ENABLED: "true"
      GF_AUTH_ANONYMOUS_ORG_ROLE: Admin
    volumes:
      - ./fleet-dashboard.json:/var/lib/grafana/dashboards/fleet.json:ro
    ports:
      - "3001:3000"
    depends_on: [prometheus]
```

with a matching `prometheus.yml` that scrapes the host-side orchestrator:

```yaml
global:
  scrape_interval: 15s

scrape_configs:
  - job_name: fleet
    metrics_path: /metrics
    static_configs:
      - targets: ["host.docker.internal:8000"] # fleet orchestrator on the host
    http_headers:
      X-API-Key:
        files: [/etc/prometheus/fleet-admin-key]
```

Put your admin key (the value fleet runs with as `ADMIN_API_KEY`) in a local
`fleet-admin-key` file next to the compose file — it is git-ignored material, so
do **not** commit it.

```sh
docker compose -f docker-compose.observability.yml up
# Prometheus → http://localhost:9090   (Status → Targets should show fleet UP)
# Grafana    → http://localhost:3001   (anonymous admin; open the "Fleet" dashboard)
```

In Grafana, add a Prometheus datasource pointing at `http://prometheus:9090`,
then the imported dashboard's `Datasource` selector will find it.

> The anonymous-admin Grafana and host-reaching Prometheus above are for **local
> development only** — do not run them like this in production.

[#176]: https://github.com/ElcanoTek/fleet/issues/176
