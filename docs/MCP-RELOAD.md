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
its whole duration, so acquiring it waits for the call to finish. A server that
still has a call in flight after a bounded timeout (30s) is force-closed (which,
for a stdio server, kills + reaps the subprocess; the blocked call then errors
out cleanly). If any new server fails to initialize, the reload rolls back the
servers it already started and leaves the live registry unchanged.

## Not yet implemented (honest scope)

- Live reload for scheduled tasks that pin an explicit MCP selection (would
  require guarding the shared `cfg.MCPServers` snapshot the scheduler reads
  per-run).
- Reload of the inline `http_tools` catalog.
