# Testing strategy

[`TESTING.md`](TESTING.md) is the map of the CI lanes: what each job runs and
how to reproduce it. This page is the reasoning behind them: what each layer of
tests can prove, where a new test belongs, and the rules a test has to follow
to be worth its runtime. Read it before adding a test; update it when the shape
of the suite changes.

## The aim

A green `CI gate` should mean the change works, and a red one should name what
broke, in minutes. That gives three properties to protect, in this order:

1. **A test can fail.** A test that cannot fail costs runtime and buys false
   confidence. Each rule below exists because a fleet test once passed over
   the bug it was written to catch.
2. **A failure points at its cause.** The cheapest layer that can see a defect
   is the one that should catch it. A browser test that catches a JSON field
   rename is a slow and vague way to learn something a unit test could have
   said in a line.
3. **The gate stays fast.** The critical path is the slowest lane, not the sum
   of all lanes. A test is cheap when it runs in parallel with something
   slower, and expensive when it lengthens the slowest lane.

## The layers

| Layer | Where | What it proves | Cost |
| --- | --- | --- | --- |
| **Unit** | `go test` packages; `web/` vitest | A decision is right: a parser, a policy, a reducer, a formatter. Pure logic, no I/O. | Milliseconds. This should be most tests. |
| **Integration** | `go test` against real Postgres (`internal/store`, `httpapi`, `sched/*`, `runner`); the governed loop against the fake LLM (`internal/agent`, `internal/taskrun`), with the host-mode sandbox | Components work together with their real dependencies: persistence, leases, governance, the turn loop. | Seconds. Postgres packages run serialized per database (see `scripts/go-test.sh`). |
| **Contract** | `testdata/contracts/`, `cmd/fleet` OpenAPI drift tests | Two processes agree on the bytes between them, checked from both sides without starting either. | Milliseconds per side. |
| **Browser, mocked** | `web/e2e/mocked` (Playwright, every `/api/*` intercepted) | The UI behaves: a click, a render, a state transition, against a deterministic backend. | About 1 s per test against one shared Next server. |
| **Live** | `web/e2e/live` and the always-on sandbox invariants in `e2e-live` | The real stack fits together: real Postgres, Go server, rootless-podman sandbox and Next build. Only the LLM is faked. | Minutes to boot. Keep it to journeys no lower layer can see. |
| **Drift** (nightly, never a gate) | `e2e-canary.yml`, `mcp-catalog-smoke.yml`, the scheduled scans, `benchmark.yml` | Things outside our control still behave: a real model, third-party MCP servers, new CVEs, throughput. | Real money or network. |

### Where does my test go?

Pick the first layer that can see the defect:

- **A decision in one function or module** → unit.
- **Behavior that depends on the database, the turn loop, or governance**
  (ceilings, audit, approvals) → integration. Drive `Manager.RunTurn` with
  `internal/fakellm`, not a mock of it, and use a real Postgres through the
  DSNs in `TESTING.md`.
- **Two processes exchanging data** (the chat event stream, an HTTP API, a
  webhook) → a contract. Don't test the boundary in a browser.
- **What the user sees and does in the UI** → mocked browser.
- **Only what needs every real piece at once** → live. Examples: the sandbox
  really executing, a Stop that really ends the server-side turn, file tools
  and bash sharing one workspace. If a lower layer could catch the bug, the
  live test is redundant.

## Contracts

When two processes talk, each side's tests used to hand-write the other side:
the web hook's tests typed SSE frames, and the TUI's tests typed SSE frames.
Those copies drift silently. The mocked chat stream had long since lost most
of the `turn.completed` fields the server really sends, and nothing noticed.

A contract replaces the hand-written copies with **one recording of the real
producer**, replayed by every consumer:

```
   internal/agent TestChatStreamContract     internal/httpapi TestChatStreamWireFraming
   (real Manager.RunTurn, scripted fake LLM)  (real turnBuffer + Attach)
                  │ records (-update)               │ records the preamble (-update) and
                  ▼                                 │ checks each turn goes on the wire
   testdata/contracts/chat-stream/*.sse  ◄──────────┘ byte for byte
   testdata/contracts/chat-stream-preamble.sse
                  │  consumers replay preamble + turn
     ┌────────────┼──────────────┬─────────────────┐
  web useTurnStream  chattui       acp translator    mocked Playwright
  (vitest replay)   Client.Stream  (Go replay)       chat.spec
```

- **Producer side.** `internal/agent/stream_contract_test.go` runs each
  scripted turn (an answer, a tool loop with a success, a non-zero exit and a
  tool error, a provider refusal, a cancelled turn, a turn fleet started
  itself after an approval card was settled) through the real
  `Manager.RunTurn` and compares the stream byte for byte with the recording.
  Only run-to-run noise is normalized (durations and the workspace path),
  with placeholders the producer could really emit. Any other change fails,
  and the failure shows the diff. Scripts avoid platform-dependent output (a
  failing command is `test -e`, which prints nothing, not an `ls` error in the
  host's locale).
- **Wire side.** `internal/httpapi/stream_contract_test.go` pushes every
  recording through the real `turnBuffer` and `Attach` and requires the bytes
  on the wire to be exactly the recorded preamble followed by the recording.
  The preamble (the synthetic `fleet.capabilities` frame `Attach` writes ahead
  of every turn) is itself recorded from the real writer. So what consumers
  replay is what production sends, not what the recorder thinks it sends:
  this test is how an HTML-escaping difference in an early normalization was
  caught.
- **Consumer side.** Every consumer replays every recording and checks what it
  shows against an oracle derived from the recording itself
  (`internal/contracttest.Expect`, and its TypeScript twin in
  `useTurnStream.contract.test.ts`): the final text, each tool call and how it
  ended, and the terminal state. The web side also declares the events and
  field types it reads (`CONSUMED`). A renamed or retyped field, or an event
  the web has never handled, fails there even after the recordings are
  regenerated.
- **Mutation-checked.** Renaming `is_err` in a recording fails the web field
  check. A hook that stops honoring `is_err` fails the web replay. A
  translator that stops honoring it fails the ACP replay.

**Changing the protocol**, in order:

1. Change the producer. `TestChatStreamContract` (or, for the stream
   preamble, `TestChatStreamWireFraming`) fails with the new stream.
2. Regenerate:
   `go test -tags fleet_host_executor ./internal/agent -run TestChatStreamContract -update`
   (and `./internal/httpapi -run TestChatStreamWireFraming -update` for the
   preamble). The package goes before `-update`: `go test` cannot tell the
   flag is boolean and would take the next word as its value.
3. Run the consumers (`make test`, `cd web && npx vitest run`). The replays
   that fail are the consumers that need updating. Update each one, and the
   web `CONSUMED` list.
4. Review the recording diff in the PR. It is the protocol change, written
   down.

**Adding a scenario**: add it to `contractScenarios()` and run step 2. Every
consumer picks it up automatically. A recording with no scenario fails the
producer test, so recordings cannot outlive their source.

**Not covered yet.** The `httpapi` envelope frames (`conversation`,
`user.message`, `history.persisted`) are emitted by the chat handler into the
same buffer, around the recorded turn, and are not in the recordings; the
replays give consumers the conversation id through the
`X-Fleet-Conversation-Id` header, as production also does. Recording them
needs a Postgres-backed `httpapi` harness. Turns
with approvals, sub-agents or reasoning need fake-LLM scripting that does not
exist yet. Until then those frames are still hand-written in the specs that
need them. The orchestrator HTTP API has its own contract: `cmd/fleet`'s
OpenAPI tests check route parity and schema drift against
`docs/openapi.yaml`.

## Rules for a test that can fail

Each rule comes from a real fleet test that once passed while proving nothing.

- **Assert the effect, not the summary.** A live spec once checked only the
  assistant's closing message, but that message renders even when a tool
  failed (the bash guard refused the command and the turn still "completed").
  Assert each tool's real output.
- **A wait that times out is a bug, not a slow test.** A `remotemcp` test held
  a mutex its own fake server needed, so every request hung for 30s and the
  "the key is refused" check passed on the timeout. Its runtime was a flat
  30.00s on every run. Pin the failure you mean (match the refusal text), and
  treat a duration that sits on a round number as a smell.
- **Never assert on wall-clock time.** Assert the decision (retry, give up,
  deliver), and shorten production timings through the test-only knobs
  described in `TESTING.md`. A test that would pass "eventually" needs a bound
  well below any watchdog that could make it pass on its own. The live Stop
  spec waits 15s for the follow-up reply, because without Stop no reply comes
  within 20s.
- **Prove the guard can fire.** When a test exists to catch a regression,
  break the code once locally and watch it fail. The PR that adds it says so.
- **Keep the oracle independent of the code under test.** `cronnext` checks
  its scheduler against a brute-force walk. The contract replays parse the
  recordings with their own small parser, not the app's. A test that computes
  its expectation with the function under test can only agree with it.
- **Race tests need concurrency, not volume.** A handful of goroutines is
  enough for the race detector to see an unsynchronized write. Dozens of
  goroutines copying large payloads only make `-race` slow. To shake out
  rarer interleavings, use `-count`.
- **A skip must be loud where it matters.** Container tests self-skip without
  rootless podman. The always-on lane that must run them (`e2e-live`) greps for
  `--- PASS` and fails on any `--- SKIP`. A new must-run test needs the same
  treatment.
- **Don't hand-write the other side of a process boundary.** Use a contract
  recording. Where none exists yet, say so in the spec.

## Keeping the gate fast

The critical path is the `-race` Go lane. What keeps it short:

- `scripts/go-test.sh` runs packages that use no database at full
  parallelism. It compiles the database-serial packages up front (`-p 1` would
  otherwise compile them on one core too), and runs `internal/httpapi` on its
  own database next to `internal/store`.
- Each Go lane keeps its own build cache, saved from `main`. A PR compiles
  only what it changed.
- Heavy jobs run side by side, not chained. Grype builds its own image instead
  of waiting on `e2e-live`.

Before adding a slow test, check which lane it lands in. A 5s test in the
independent Go packages costs the gate nothing, because it runs in parallel. A
5s test in a database-serial package adds to the slowest lane. So does a 5s
step in `e2e-live`.
