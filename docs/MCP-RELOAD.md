# MCP server hot-reload (#218)

A running fleet can add, remove, or update **MCP servers** without a process
restart. An operator edits the client-config bundle's `manifest.yaml` (the MCP
catalog) and triggers a reload; fleet diffs the new catalog against the live
registry and applies the **minimum** set of changes — starting newly-added
servers, draining + closing removed ones, and restarting changed ones — while
leaving unchanged servers (and their live subprocesses / connections) untouched.
No active conversation is interrupted.

This complements config hot-reload ([`CONFIG-RELOAD.md`](CONFIG-RELOAD.md)):
that reloads scalar settings; this reloads the tool catalog.

## How to trigger a reload

Three mechanisms, all equivalent:

1. **CLI** — `fleet mcp reload` (pretty-prints the summary). Uses `ADMIN_API_KEY`
   and `FLEET_ORCHESTRATOR_ADDR` by default; `--server`, `--admin-key`, and
   `--json` override.
2. **Signal** — `kill -HUP <fleet-pid>`. `SIGHUP` is the canonical "reload
   configuration" signal and is deliberately left free by config reload (which
   uses `SIGUSR2`). The outcome is logged.
3. **HTTP** — `POST /admin/mcp-servers/reload` (admin-API-key gated). Returns a
   JSON summary:

   ```json
   {
     "added":     ["newserver"],
     "removed":   ["retiredserver"],
     "restarted": ["changedserver"],
     "unchanged": ["stableserver"]
   }
   ```

## What a reload does — and does not — cover

**Covered (takes effect without a restart):**

- The **interactive chat** MCP catalog: the change is visible on the *next* chat
  turn (each turn rebuilds its tool roster from the live registry). In-flight
  turns finish on their current tool set.
- **Scheduled tasks that use the shared catalog** (no explicit per-task MCP
  selection): they run against the same reloaded client, so the *next* run picks
  up the change.
- The **settings picker** (optional-server catalog) and the per-server tool
  allowlists / optional gating are refreshed atomically alongside the client, so
  a newly-added *optional* server is correctly gated rather than always-on.

**Not covered (still needs a restart):**

- **Scheduled tasks that pin an explicit MCP selection** build a fresh per-run
  client from the boot-time catalog snapshot (`cfg.MCPServers`), which is read
  concurrently by the scheduler and is therefore not mutated by a reload. Such a
  task sees manifest changes on the next process restart.
- **Inline HTTP tools** (`http_tools`, #261) — the synthetic tools server is left
  untouched by a reload.
- **Per-user remote MCP connections** (#443/#449) are built fresh per turn and
  are unaffected (they were never part of the shared catalog).
- A server whose manifest entry references a **brand-new environment variable**
  not present at boot: the value resolves from the process environment at reload
  time, but a var the operator must also add to the environment still requires
  whatever mechanism sets that env var.

## Concurrency + draining

`Reload` is safe to call while tool calls are in flight. New servers are built
and initialized *outside* the registry lock (a subprocess spawn / HTTP handshake
can block); the registry map is then swapped under a brief write lock; and each
retired server is drained under its own lock — a tool call holds that lock for
its whole duration, so acquiring it waits for the call to finish (a graceful
drain). The wait is bounded because every transport call respects its context
(the stdio transport selects on `ctx.Done`; the HTTP transport uses a bounded
client). A retired server is marked so `callTool` refuses a late call rather than
resurrecting a killed stdio subprocess via its dead-transport restart path. If a
new server fails to initialize, the reload rolls back the servers it already
started and leaves the live registry unchanged.

Reloads are **serialized** end-to-end (both at the client and at the manager
level), so two triggers firing at once (e.g. `kill -HUP` during a `fleet mcp
reload`) run one after another rather than interleaving into a state where the
live client and the published tool-gating describe different manifests.

A newly-added *optional* server's gating is published **before** its tools go
live, so it can never be briefly treated as always-on (which could otherwise
push a near-full turn past the 128-tool ceiling). One narrow residual remains: a
turn is single-threaded here but reads its optional-server gating at turn start
and the live tool catalog slightly later, so a turn whose start straddles a
reload may, for that one turn, see a just-*removed* optional server's tools; it
self-heals on the next turn. Removing a server never adds tools, so it cannot
cause a ceiling failure.

## Not yet implemented (honest scope)

- Live reload for scheduled tasks that pin an explicit MCP selection (would
  require guarding the shared `cfg.MCPServers` snapshot the scheduler reads
  per-run).
- Reload of the inline `http_tools` catalog.
