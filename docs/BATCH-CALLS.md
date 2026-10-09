# Batch MCP calls: timeout scaling, the single-record canary, and raw results (#1712)

Design note for how the agent loop treats a server-side batch call: an MCP
call whose input carries a non-empty `deal_ids` array (the bundle servers'
`*_merge_deal_*` family, which take `deal_ids` plus `values_file` /
`values_sha256` and answer `{"results": [{deal_id, success, …}]}`). Fleet's
agentcore is a port of Cutlass's orchestration; this note records which of
Cutlass's batch protections were ported, how Fleet's version differs, and
what is still open.

## Why

The Tunnl ELC07362 run (2026-09-30, a 90-deal OpenX block-list update) hit
three gaps at once:

1. A 59-deal merge ran past the flat 5-minute MCP call timeout. The stdio
   server was restarted mid-batch and the agent re-ran the remainder in
   chunks. The deadline also started *before* the call queued for the
   per-server MCP mutex, so time spent waiting behind another call came out
   of the call's own budget.
2. Nothing stopped an agent from applying an unproven operation to a whole
   batch in one call. The update protocol's `canary_gate` was prose only.
3. The policy recorded the model-visible output, which is capped at 64 KiB. A
   large `results[]` arrived as a truncation envelope, was not parsed, and fell
   through to the single-call success path. A malformed `results[]` fell
   through the same way.

## What shipped

### Batch timeout scaling

- A call with a non-empty `deal_ids` gets **20 s per listed record** by default,
  floored at the ordinary 5-minute MCP call timeout and capped at 30 minutes
  (`toolCallTimeoutFor`, `internal/agentcore/mcp_tools.go`; port of cutlass
  `toolCallTimeoutForServer`, cutlass#1067).
- **The per-record pace is bundle data.** A server whose upstream is slower per
  record, for example because a per-minute rate limit paces every request,
  declares its own pace on its manifest entry:

  ```yaml
  mcp_servers:
    - name: nexxen_mcp
      command: …
      batch_seconds_per_deal: 45   # 20 req/min per API user, ~5 calls per deal
  ```

  The value is in seconds, 1–1800. The loader rejects anything outside that
  range, so `fleet validate-config` reports it too. A larger per-record value
  could never apply under the 30-minute cap, and is most likely milliseconds
  typed as seconds. When the key is absent, the server gets the 20 s default.
  `Bundle.AgentPolicy()` collects the declarations keyed by server name, and
  `agentcore.ConfigureAgentPolicy` installs them. A registered named-account
  variant (`<server>_<account>`) resolves to its base server's value through
  the one server-name keying rule (`longestServerKey`), and its own entry wins
  when it has one. The engine matches no server by name.
- **The budget starts once the call holds the server mutex.** `mcpTool` attaches
  the budget to the call's context with `mcp.WithCallTimeout`, and
  `mcp.Server.callTool` starts it only after taking the server's mutex, so
  queueing behind another call on the same server does not eat the budget. A
  call whose deadline ran out while it was queued is refused **before it is
  sent**. It is never written to a stdio server on a dead context.
- **The budget crosses the broker.** In production the parent reaches MCP
  servers through the out-of-process broker (`internal/mcpbroker`), and a
  context value does not cross that pipe. The broker client copies the budget
  into the call request (`callTimeoutMs`), and the credential-owning child puts
  it back on the call's context before the call reaches `mcp.Server.callTool`.
  `TestBrokerScope_CallBudgetCrossesTheWire` (`cmd/fleet`) drives that whole
  path against a real stdio server.
- `mcpTool` also keeps an outer deadline of **budget + 5 minutes**. It is the
  backstop for a broker that never reaches an `mcp.Server`, and for the mutex
  queue itself.

### Single-record canary before a multi-record batch

`internal/agentcore/batch_canary.go` ports cutlass `canaryKey` / `canaryShape` /
`creditBatchCanary` and the `checkBatchBinding` tail (cutlass#740.7, 3d1546bf,
9c532cee, #1081).

- A `deal_ids` batch of more than one record is refused until a one-record
  application of the same critical action has succeeded this run. The
  one-record application must use the same value set and operation shape.
  - **Same value set** means the same `values_sha256`, or a canonical digest of
    the inline `values`.
  - **Same operation shape** means the same value for **every other argument**,
    except record addressing (`deal_ids`, the single-record id keys,
    `deal_references`), value transport already bound by the digest, `verbose`
    and a per-record `etag`. An argument the engine does not recognize, such
    as `dry_run`, `is_excluded`, a seat like `member_id`, or a per-dimension
    `countries_include` list, therefore needs its own canary.
- Credit comes from a one-record batch whose **requested** record reported
  success, or from a successful single-record call.
- The key is `CriticalActionKey`, which is the server/variant prefix plus the
  alias class. `critical_tool_aliases` twins on one server therefore share a
  canary, while two servers or two client-variant seats never do.
- The block message says that a one-record batch with the matching digest *is*
  the canary (cutlass 4a6f3362).

### The policy reads raw results; a malformed `results[]` fails closed

- On a successful call, the policy now records the governed text from
  *before* the model-visible boundary: redacted, screened, with post-hook
  fragments appended (`policyResultText`, `tool_call_framing.go`). That text
  is capped at 8 MiB + 1 byte. The model, the journal and the log still get the
  bounded bytes. A 200-deal `results[]` is now discharged record by record
  instead of arriving as a truncation envelope.
- `malformedDealOutcomes` (`batch_results.go`, port of the cutlass function of
  the same name) makes a call **failed** when its `results[]` is present but
  malformed, or when its body is too large or cannot be decoded. A failed call
  discharges nothing, earns no canary credit, and is charged to the retry
  budget.

## Deviations from Cutlass

- **Pace by declaration, not by name.** Cutlass gives `nexxen_mcp*` 45 s per
  record in code. Fleet reads `batch_seconds_per_deal` from the bundle, under
  the engine/bundle doctrine in `AGENTS.md`. Until a bundle declares it, a
  Nexxen batch gets the 20 s default. The 5-minute floor still covers up to 15
  records.
- **The canary shape is an exclusion list.** Cutlass binds only a fixed
  list of mode words (`merge_mode`, `list_type`, …). The bundle servers carry
  operation-changing arguments outside that list, such as `is_excluded`,
  `member_id`, `logged_in_owner_id` and per-dimension include/exclude lists.
  Under the fixed list, a canary of one operation could unlock a batch of
  another. Fleet binds every argument except a short engine-owned exclusion
  list, so an unknown argument fails closed.
- **The budget starts after the server mutex** and travels across the broker
  wire. Cutlass has neither the per-server mutex queue nor the
  process-separated broker.
- **The canary is keyed by alias class** (Fleet's `critical_tool_aliases`,
  #1604). A one-record canary through `X_upload` covers a batch through `X` on
  the same server with the same digest and shape.
- **Failed calls keep recording the bounded bytes.** This preserves the #793
  invariant that the policy, journal, log and model see the same text for a
  failure. The bundle `deal_batch` tools report partial failure as a normal
  `{"success": false, "results": [...]}` rather than `isError`, so a partial
  batch still reaches the policy raw.

## Deferred

- **Nexxen's pace in the bundle.** elcano-config (and every other bundle that
  ships a Nexxen server) should add `batch_seconds_per_deal: 45` to its
  `nexxen_mcp` entry **after** the Fleet release carrying this change is
  deployed. The manifest decoder is strict, so an older Fleet refuses a bundle
  that carries the new key.
- `mcp.Server.callTool`'s mutex is not context-aware, so a queued call cannot be
  cancelled while it waits. The outer backstop ends the wait, and the call is
  then refused before it is sent.
- HTTP MCP transports keep an `http.Client.Timeout` of 2 minutes, which caps an
  HTTP-served batch below its scaled budget. The SSP bundle servers are stdio,
  so this does not affect them today.
- The malformed-results branch does not touch create-commitment settlement.
  If the concurrent settled-failed-create work adds an
  `unsettleCreateFailure`, Cutlass also calls it from that branch.
- `parseDealOutcomes` still uses its own `8*1024*1024` literal instead of
  `maxDealOutcomesBytes`.
