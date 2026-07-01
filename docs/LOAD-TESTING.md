# Load testing & benchmarks (#296)

Fleet is a single-big-box deployment, so capacity is finite and worth measuring.
This gives you two tools: an HTTP load generator (`fleet-bench`) for end-to-end
chat throughput/latency, and subsystem Go benchmarks for the scheduler and SSE
fan-out hot paths.

Everything runs against the **fake-LLM seam** (`internal/fakellm`), so a load run
costs nothing and is deterministic — no real provider calls.

## `fleet-bench` — HTTP chat load

Build it (it is a dev/ops tool, not part of the deployed runtime, so `make
build`/`make install` do not emit it):

```sh
make fleet-bench     # → ./fleet-bench
```

Point a fleet instance at the fake LLM (`OPENROUTER_BASE_URL=http://127.0.0.1:18090`,
the address `cmd/fake-llm` listens on), then drive chat turns:

```sh
export FLEET_SERVER_TOKEN=…            # the shared X-Chat-Server-Token
./fleet-bench chat \
  --server http://127.0.0.1:8080 \
  --email you@org \
  --concurrency 20 \
  --duration 60s \
  --message "[[echo:load-test reply]] respond briefly"
```

It spawns `--concurrency` workers, each posting `POST /chat` and reading the SSE
stream to its terminal event (`turn.completed`/`cancelled`/`error`) — so it
measures **full-turn** latency, not just HTTP connect time. Turns cut short by
`--duration` are the graceful stop, not counted as errors. The report:

```
fleet-bench chat — http://127.0.0.1:8080, 20 workers, 1m0s
  turns:        1843 ok, 0 errors
  throughput:   30.7 turns/sec (wall 1m0s)
  latency p50:  612ms
  latency p95:  1.1s
  latency p99:  1.4s
  latency mean: 640ms
  goroutine Δ:  +0 (net, after GC — a large positive Δ hints at a per-turn leak)
  heap Δ:       +1.2MB bytes (after GC)
```

The `[[echo:TEXT]]` marker makes the fake LLM stream back exactly `TEXT`
regardless of turn index; use a `[[scenario:NAME]]` marker to exercise tool
calls / retries (see `internal/fakellm/scenarios.go`).

`scripts/e2e-boot-server.sh` stands up Postgres + fake-llm + fleet together (it
already wires `OPENROUTER_BASE_URL` and seeds a test user) — the quickest way to
get a target instance for a load run.

## Subsystem benchmarks

Focused Go benchmarks for the two highest-leverage throughput paths:

- **`BenchmarkClaimNextPendingTask`** (`internal/sched/db`) — the scheduler's
  `FOR UPDATE SKIP LOCKED` claim transaction each worker runs. Needs a sched test
  DB (`DATABASE_URL`).
- **`BenchmarkSSEFanOut`** (`internal/httpapi`) — the turn buffer's per-event
  fan-out to N concurrent SSE subscribers (the chat-concurrency hot path).
  In-process, no DB.

```sh
# scheduler claim throughput (needs a sched Postgres)
DATABASE_URL=postgres://…/fleet_sched_test go test -run '^$' \
  -bench BenchmarkClaimNextPendingTask -benchmem ./internal/sched/db/

# SSE fan-out (1/10/100 subscribers)
go test -run '^$' -bench BenchmarkSSEFanOut -benchmem ./internal/httpapi/
```

The [`Weekly benchmarks`](../.github/workflows/benchmark.yml) workflow runs both
on a schedule (and on manual dispatch) and prints the results — non-blocking, so
timing noise never gates a PR.
