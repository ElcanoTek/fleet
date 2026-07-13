# Testing MCP servers

How to verify that the MCP servers a fleet deployment depends on actually
work — from a one-command smoke test to full end-to-end lanes.

## `fleet mcp test` — per-server smoke test

```sh
fleet mcp test --all                 # handshake every enabled catalog server
fleet mcp test gamma delta           # just these two
fleet mcp test --all --json          # machine-readable (CI gates)
fleet mcp test gamma --timeout 10s   # per-server handshake budget (default 30s)
```

For each requested server it loads the bundle **through the same loader the
server boots with** (`clientconfig.Load` → `MCPServerConfigs`: env
interpolation, the `enabled_env`/`enabled_groups` gate, TLS hardening, and the
bundle-root working directory are identical to a real boot), spawns or
connects the server **exactly as the broker would** (same `internal/mcp`
client, same `${FLEET_WORKSPACE}` expansion — one credential path, #167), and
runs the MCP handshake: `initialize` + `tools/list`.

Output per server: connected or not, tool count, tool names, and on failure
the actionable error — a missing executable, an unset credential, a dead
process, a handshake timeout — the same classes that otherwise surface as
cryptic mid-chat tool errors. Requesting a server whose enable gate is off
says so explicitly (vs. "unknown server" for a typo).

Exit codes: `0` all requested servers connected · `1` at least one failed
(or a requested name is unknown/gated off) · `2` usage error.

### What it needs (and deliberately does not)

- **No Postgres, no running fleet server, no web tier, no Podman.** Bundle
  MCP servers run host-side under the broker, so the probe spawns them the
  same way. Like `validate-config`, it boots nothing.
- **It does need** the bundle checkout, each server's runtime (e.g. Python
  and its deps for Python servers), and the real credentials in the process
  env. Run it where the deployment's env lives — the deploy box, or a CI job
  with staging credentials. That is the point: it validates the exact
  machine-plus-credentials combination production uses.
- It never prints a credential value.

### Honest scope

- The probe is **point-in-time liveness + tool discovery**. It does not call
  tools, so a server that lists tools but fails on invocation (e.g. a key
  that authenticates but lacks a scope) still passes. Use an eval
  (`docs/EVALS.md`) or a manual tool call for that depth.
- HTTP catalog servers are probed with the manifest's resolved headers/TLS;
  per-user hosted connectors (Settings → Connections) are NOT covered here —
  they get their own add-time validation handshake
  (`docs/CONNECTOR-ONBOARDING.md`).

## The rest of the testing ladder

| Layer | Command / surface | What it proves |
| --- | --- | --- |
| Manifest shape | `fleet validate-config` | Bundle/manifest well-formed, env keys named, executables present |
| Server liveness | `fleet mcp test --all` | Each server spawns, handshakes, lists tools with real credentials |
| Runtime state | admin Health panel / `GET /health` | What the *running* deployment actually connected |
| Hot iteration | `fleet mcp reload` (`docs/MCP-RELOAD.md`) | Bundle edits re-register without a restart |
| Full stack | live Playwright lane (`npm run test:e2e:live`) | Real server + sandbox + broker wiring, fake LLM |
| Model-in-loop | `fleet eval` (`docs/EVALS.md`) + nightly canary | The model can actually drive the tools |

For a quick end-to-end sanity of one server in production: enable it in a
chat, ask the model to call one specific tool, and check
`GET /conversations/{id}/audit` — the persistent tool-call audit log shows
exactly what ran and what came back.

Bundle repos can run `fleet mcp test --all --json` in their own CI against
staging credentials, so a broken server sync or a manifest env-key typo fails
the bundle PR instead of a customer's chat turn (per the engine/bundle
boundary in `CLAUDE.md`, contract checks live with the bundle).
