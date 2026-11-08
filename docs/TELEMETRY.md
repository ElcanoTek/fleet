# Telemetry — structured, correlated logs and traces through the agent loop

Issue #2052. Decision record: [ADR-0102](adr/0102-structured-logs-and-traces-across-the-broker.md).

This page is both the design note (what shipped, how it deviates from the
issue) and the operator guide (how to read the logs, how to turn tracing on,
a minimal local Collector setup).

## What shipped

### 1. One log format, carrying the ids

Every server log line is a `log/slog` record. Fleet's own code never writes
through the standard library `log` package: a `forbidigo` rule in
`.golangci.yml` bans `log.Print*`, `log.Fatal*` and `log.Panic*` everywhere,
tests included. The one allowlisted file is
`internal/logging/logging_test.go`, which calls `log.Printf` on purpose to prove
that a third-party library's line is bridged into the same stream.

All ~880 former `log.Printf`/`Println`/`Print`/`Fatal*` calls (171 files) were
converted. The interpolated values became attributes (`task_id`, `conv_id`,
`err`, `server`, …); the message keeps the line's wording without them; the
level now says what the line is (`ERROR` for a failure or an error value, `WARN`
for a warning, fallback, skip or retry, `INFO` otherwise). `log.Fatal*` became
`logging.Fatal`, which logs at error and exits.

**Format.** `FLEET_LOG_FORMAT=json` (the default) writes one JSON object per
line:

```json
{"time":"2026-11-07T12:00:01.2Z","level":"WARN","msg":"remote-mcp: skipping server — token unavailable","conv_id":"9b1…","turn_id":"4f2…","user_id":"ana@example.com","request_id":"0af7…","trace_id":"0af7651916cd43dd8448eb211c80319c","span_id":"b7ad6b7169203331","reg_name":"github","user":"ana@example.com"}
```

`FLEET_LOG_FORMAT=text` writes the same record as logfmt, for reading
`journalctl` by eye:

```
time=2026-11-07T12:00:01.2Z level=WARN msg="remote-mcp: skipping server — token unavailable" conv_id=9b1… turn_id=4f2… reg_name=github
```

Both go to stderr (journald under the systemd unit) and, with `FLEET_LOG_FILE`,
also to the rotating file. `FLEET_LOG_LEVEL` gates every fleet line in both
formats. A line a third-party library writes through `log.Printf` is bridged
in at `INFO` and is not gated, so raising the level cannot hide a library's
only report of a failure. The MCP broker child logs in the same format and
level. It opens no log file of its own (two processes rotating one file would
corrupt it): the parent pipes the child's stderr, whole lines at a time, into
its own destination, so broker lines reach stderr and `FLEET_LOG_FILE` alike.

**Correlation ids.** `logging.From(ctx)` returns the default logger with the
context's ids attached. The ids are attached where they become known:

| Key | Attached at | Value |
|---|---|---|
| `request_id` | `otelsetup.Middleware` (every HTTP request) | the `X-Request-Id` the response carries |
| `user_id` | `httpapi` auth gate, sched auth middleware | the signed-in email or username; for an API-key call, `apikey:<key id>`, never the key |
| `conv_id`, `turn_id` | the chat turn's context (`httpapi/chat.go`) | the conversation and turn |
| `task_id`, `run_id` | `runner.executeTask` | the task and its run (the claim's lease token, unique per attempt) |
| `trace_id`, `span_id` | read from the context's live span, never stored | present only when tracing is on |

A chat turn's context is detached from its HTTP request's lifetime, but it
keeps the request's ids and continues its trace. When a call site passes an id
the context also carries, the call site's value wins, so a JSON line never
holds a key twice. `agentcore.Run` adds `conv_id`/`task_id` itself, so a
driver that forgets still gets them.

**Redaction.** Both handlers install one `ReplaceAttr` hook
(`internal/logging/redact.go`). It reuses the existing rules and adds none of
its own:

- the `internal/redact` canonical patterns (vendor key prefixes, bearer and
  basic auth, PEM blocks, `api_key=`/`token:`-style markers), applied to the
  message, every string value, every error and every `fmt.Stringer`;
- the same literal set the tool-output scrubber (`agentcore/redact.go`) and
  the broker scrubber (`mcpbroker/redact.go`) hold: their env-literal
  snapshots and every runtime `RegisterSecretLiteral(s)` call are forwarded to
  the log redactor, so a connector credential of any shape is scrubbed from a
  log attribute by value;
- the key rule `internal/logging` already had (`secret`, `token`, `password`,
  `api_key`, `authorization`, `bearer`, `credential`): a string value under
  such a key is blanked whole. Numbers are left alone, so `prompt_tokens=1234`
  survives.

### 2. Spans through the loop

Every span uses the tracer in `internal/telemetry` and follows two rules.
Attributes are ids and sizes only: never prompt or completion text, tool
arguments or results, commands, file contents or paths, queries, or an error
message. An error is recorded by class only (`*url.Error`, `canceled`,
`tool_error`), and the log line for the same event, which carries the span's
`trace_id`, holds the redacted detail.

| Span | Where | Attributes (besides ids) |
|---|---|---|
| `chat.turn` | `httpapi.runTurnAsync` | conv/turn id |
| `task.run` | `runner.executeTask` | task/run id, attempt; error class of the run |
| `agentcore.run` | `agentcore.Run` | run mode, conv/turn/task/run id |
| `model.stream` / `model.generate` / `model.*_object` | `tracedModel`, wrapped around every model `buildAgent` uses (primary and fallbacks) | `gen_ai.system`, `gen_ai.request.model`, message and tool counts, input/output/cache-read tokens, finish reason |
| `tool.call` | `panicContainedTool.Run`, the outermost dispatch boundary | `gen_ai.tool.name`, call id, input/output bytes, `is_error` |
| `sandbox.exec` | `Sandbox.RunBash`/`RunPython`/`RunFileOp`, the front door every backend shares (podman, kubernetes, host) | op (`bash`, `python`, `fileop.write`, …), backend, timeout, exit code or status, input/output bytes |
| `mcpbroker.call` (client) | `mcpbroker.Client.callMCP`, in the parent | server, tool, scoped |
| `mcpbroker.call` / `.scope_open` / … (server) | the broker child's request loop | method, server, tool, scoped, `is_error` |
| `db.tx <store method>` | every transaction of `internal/store` and `internal/sched/db` | `db.system.name`, isolation, read-only, commit/rollback, `code.function.name` |

So one chat turn is one trace: the HTTP request → `chat.turn` →
`agentcore.run` → `model.stream` steps and `tool.call`s → each tool's
`sandbox.exec`, `mcpbroker.call` (parent, then child) and `db.tx` spans.
A scheduled run's trace is rooted at `task.run`.

**DB transactions.** Both stores call `*sql.DB.BeginTx` in about 85 places, so
the transaction seam used here is the driver. `internal/dbtrace` opens the pool
on pgx's `database/sql` driver and wraps each connection. Its `BeginTx` starts
the span and the transaction's `Commit`/`Rollback` ends it, so no call site
changed. The span is named after the store method that opened the transaction.
That lookup runs only when the span is recording.

**Across the broker.** The broker is a separate process, and a context cannot
cross its stdio pipe. Each request frame therefore carries two new optional
fields:

- `trace`: the W3C `traceparent`/`tracestate` of the parent's client span;
- `log`: the correlation ids.

The child extracts `trace`, so its server span, and the MCP server call under
it, joins the turn's trace. It exports those spans with its own OTLP exporter:
same endpoint and TLS settings, `service.name` from `FLEET_SERVICE_NAME`, and
`fleet.process=mcp-broker`. The child accepts only the six known `log` keys and
drops any other key, so the parent cannot inject arbitrary attributes into the
credential owner's log. Both fields hold public identifiers only, so the
frame's rule that credentials are never on the wire is unchanged.
`TestBrokerTracePropagation_RealSubprocess` proves this against a real child
process that has its own tracer provider.

**Overhead when off.** With `FLEET_OTEL_ENDPOINT` unset the global provider is
OpenTelemetry's no-op. Every `Start` returns a non-recording span, and all
attribute work, including the DB caller lookup, sits behind
`span.IsRecording()`. `trace_id` is then absent from logs and from the UI; the
turn and run ids still correlate.

### 3. OTLP over TLS

| Setting | Meaning |
|---|---|
| `FLEET_OTEL_ENDPOINT` | the collector, as `host:port` or a URL (`https://otel.internal:4317`); unset leaves tracing off |
| `FLEET_OTEL_CA_FILE` | PEM bundle that verifies the collector; unset uses the system roots |
| `FLEET_OTEL_CLIENT_CERT_FILE` + `FLEET_OTEL_CLIENT_KEY_FILE` | client certificate for mTLS (both or neither) |
| `FLEET_OTEL_INSECURE` | plaintext; **refused unless the endpoint is loopback** (`localhost`, `127.0.0.0/8`, `::1`) |
| `FLEET_OTEL_SAMPLE_RATIO` | parent-based head sampling ratio (unchanged) |

Export is TLS 1.2+ by default. A URL's scheme never chooses plaintext: only
`FLEET_OTEL_INSECURE` does. The following configurations fail `Init`, so the
process logs `otel: init failed, tracing disabled` and runs untraced:

- plaintext to a remote host;
- `FLEET_OTEL_INSECURE` together with any TLS setting;
- half a client key pair;
- an unreadable CA file, or one with no certificate in it.

The settings are in the configuration registry and
[`CONFIGURATION.md`](CONFIGURATION.md).

### 4. From the UI to the logs and the trace

- **Admin run log.** The Operations Center's run log (`LogViewer`) shows the
  run's **Run ID** and, when tracing is on, its **Trace ID** under the token
  and cost strip, each with a **Copy** button. Both are recorded on the
  persisted run log (`LogSession.run_id` / `trace_id`). The guide
  (`operations-center.md`) says where they are.
- **Chat turns.** The SSE `turn.started` event carries `trace_id` when the turn
  is traced. Every log line of the turn carries `turn_id`, which the existing
  `X-Fleet-Turn-Id` response header also returns.

## Operating it

**Find a turn's lines.**

```sh
journalctl -u fleet -o cat | jq -c 'select(.turn_id=="4f2…")'
journalctl -u fleet -o cat | jq -c 'select(.trace_id=="0af7…") | {time,level,msg}'
```

**A minimal local Collector to Tempo or Jaeger.** The OpenTelemetry Collector
runs beside fleet, on loopback, and forwards to the backend. fleet talks
plaintext to it only because the Collector is on loopback:

```sh
# /etc/fleet/fleet.env
FLEET_OTEL_ENDPOINT=127.0.0.1:4317
FLEET_OTEL_INSECURE=true
```

```yaml
# otel-collector.yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 127.0.0.1:4317
exporters:
  otlp/jaeger:            # Jaeger all-in-one accepts OTLP; for Tempo use its OTLP gRPC port
    endpoint: 127.0.0.1:14317
    tls:
      insecure: true      # loopback on this host; use real TLS across hosts
processors:
  batch: {}
service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp/jaeger]
```

```yaml
# docker-compose.yaml (Collector + Jaeger UI on :16686)
services:
  otel-collector:
    image: otel/opentelemetry-collector-contrib:0.115.0
    command: ["--config=/etc/otelcol/config.yaml"]
    volumes: ["./otel-collector.yaml:/etc/otelcol/config.yaml:ro"]
    network_mode: host     # so it listens on the host's 127.0.0.1:4317
  jaeger:
    image: jaegertracing/all-in-one:1.62.0
    ports: ["127.0.0.1:16686:16686", "127.0.0.1:14317:4317"]
```

For Tempo, point the exporter at Tempo's OTLP gRPC receiver instead and view
the traces in Grafana's Tempo data source. A trace id copied from the run log
goes straight into Jaeger's or Grafana's trace search.

**A remote collector over TLS or mTLS.** Leave `FLEET_OTEL_INSECURE` unset and:

```sh
FLEET_OTEL_ENDPOINT=otel.internal.example.com:4317
FLEET_OTEL_CA_FILE=/etc/fleet/otel-ca.pem
FLEET_OTEL_CLIENT_CERT_FILE=/etc/fleet/otel-client.pem   # mTLS only
FLEET_OTEL_CLIENT_KEY_FILE=/etc/fleet/otel-client.key    # mTLS only
```

On the Collector, set `receivers.otlp.protocols.grpc.tls.cert_file`/`key_file`,
and `client_ca_file` to require the client certificate.

## The issue's design questions

- **One pipeline, or logs in journald with trace ids?** Logs stay in journald
  (and the optional file) with `trace_id`/`span_id` attached; they are not
  shipped as OTel logs. Operators already parse that stream, it works with
  tracing off, and a JSON line is ingestible by any log pipeline, including an
  OTel Collector `filelog`/`journald` receiver for anyone who wants one
  backend.
- **Sampling for long-running scheduled tasks.** Head sampling, parent-based,
  decided once at the root: `task.run` for a scheduled run and the HTTP request
  or `chat.turn` for chat. A run is therefore traced whole or not at all, and
  the broker child follows the parent's decision through `traceparent`. Spans
  are exported in batches as each one ends, so a run that takes hours streams
  its model, tool and DB spans while it runs. Only the root span arrives at the
  end. `FLEET_OTEL_SAMPLE_RATIO` sets the share of roots that are sampled.

## Deviations and behaviour changes

- **Plaintext OTLP is no longer the default.** A deployment that pointed
  `FLEET_OTEL_ENDPOINT` at a plaintext Collector must now set
  `FLEET_OTEL_INSECURE=true`, and only a loopback endpoint may do so.
  Otherwise the TLS handshake fails, and the Collector's address appears in the
  `otel: export error` warnings.
- **`FLEET_LOG_FORMAT=text` is logfmt,** not the old `2006/01/02 15:04:05 msg`
  lines, which no longer exist.
- **Message text changed shape.** Values moved out of the message into
  attributes. Docs that quoted log lines were updated, and so were the tests
  that matched them. No script under `scripts/` matched a log line.
- **`FLEET_LOG_LEVEL` now gates every fleet line.** Before the migration it
  gated only native slog lines. A third-party library's bridged line is still
  ungated.
- **The trace's `service.name` reads `FLEET_SERVICE_NAME`,** as the registry
  always said; it was hard-coded `fleet`.
- **The UI correlation is in the admin run log.** The chat UI does not show a
  trace id; a chat turn's trace id is on its `turn.started` event and on every
  log line of the turn.

## What it does not do

- A trace does not cover the model provider's HTTP request. The `model.*` span
  times the call from fleet's side.
- An in-process MCP call (no broker child, e.g. tests) is traced by its
  `tool.call` span only.
- `trace_id` exists only when tracing is on. With it off, `turn_id`/`run_id`
  are the correlation keys.
