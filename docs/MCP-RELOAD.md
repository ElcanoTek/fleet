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
  selection): the next run expands against the broker's refreshed public server
  inventory and opens a new child-owned scope.
- **Scheduled tasks that pin an explicit MCP selection:** the next run opens its
  child-owned scope against the refreshed bundle bases. An already-running task
  keeps the scope snapshot it started with.
- The **settings picker** (optional-server catalog) and the per-server tool
  allowlists / optional gating are refreshed atomically alongside the client, so
  a newly-added *optional* server is correctly gated rather than always-on.

**Not covered (still needs a restart):**

- **Inline HTTP tools** (`http_tools`, #261) — the synthetic tools server is left
  untouched by a reload.
- **Per-user remote MCP connections** (#443/#449) are built fresh per turn and
  are unaffected (they were never part of the shared catalog).
- **Connector environment changes**, including rotation, deletion, a new
  account-suffixed seat, or a brand-new variable. The broker intentionally uses
  its boot environment snapshot; restart Fleet to transfer a new snapshot and
  re-scrub the parent.

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

The child applies its registry diff first and returns one self-describing public
snapshot. The parent then publishes catalog, accounts, allowlists, optional
gates, prompt roster, picker metadata, and scheduled server inventory from that
result; the reload endpoint does not report success before publication. A run
that already owns a scope remains on its old snapshot. A run starting during the
brief child-response/parent-publication handoff can only select from the public
snapshot its driver already held; it does not receive credential-bearing child
definitions or bypass the normal selection gate.

## Not yet implemented (honest scope)

- Reload of the inline `http_tools` catalog.
