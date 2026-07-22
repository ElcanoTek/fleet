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

### `--deep` — verify credentials against the upstream

```sh
fleet mcp test --all --deep
```

The handshake proves dial tone; `--deep` proves the far end accepts the
call. For every connected server that advertises an auth-status tool
(`auth_status` or `*_auth_status` — the bundle servers' convention for "ask
the upstream whether my credentials are actually valid"), `--deep` CALLS
that tool and reports the outcome under the server's row:

```
✓ magnite_mcp              stdio   32 tools  (optional)
    deep ✓ magnite_auth_status — authenticated: seat 12345 ok
✗-style failure:
    deep ✗ ix_auth_status FAILED — 401 Unauthorized: key revoked
```

A failing deep check fails the run (exit 1) even though the server itself
connected. A server with no auth-status tool is noted and skipped — never
failed. Honest scope: the probe trusts the MCP error flag (`isError`) and
surfaces the tool's own text; it does NOT parse result content for
auth semantics — a server that reports "not authenticated" as a NON-error
result will read as deep-✓, so glance at the surfaced text, don't just read
the glyph.

### `probe:` — a declared read-only canary call

Auth-status proves the upstream accepts the credentials; a **probe** proves
the round trip returns real data. Each catalog server may declare ONE
read-only canary call in the manifest, which `--deep` executes after the
auth-status checks:

```yaml
mcp_servers:
  - name: mail
    # …
    probe:
      tool: list_messages        # must be advertised (and inside tools: when set)
      args: {maxResults: 1}      # literal call arguments — keep them minimal
      contains: messages         # optional substring assertion on the result text
```

```
✓ mail                     stdio   12 tools
    deep ✓ mail_auth_status — authenticated as ops@example.com
    probe ✓ list_messages — {"messages": [{"id": "18c…"}], …}
```

The probe fails (and fails the run) when the tool is not advertised, the
call errors, the result is flagged `isError`, or the `contains:` substring is
missing. A server with no `probe:` is never failed for it — with neither an
auth-status tool nor a probe, the skipped note says so, so the report shows
which servers are unproven beyond the handshake rather than hiding the gap.

**The safety model is declare-and-vet**: the runner only ever calls the
exact tool + args a manifest declares — it never auto-discovers tools to
call — so the bundle author vets each canary ONCE for side effects when
declaring it. Only declare calls that are genuinely read-only and idempotent
upstream (beware reads with side effects: marking items seen, audit noise,
rate-limit burn). Mutating tools (send/create/submit) must never get a probe.
Keep `contains:` to stable, shape-ish markers — asserting on live content
makes the canary flaky as real-world state changes; the probe proves the pipe
works, not that the data looks a particular way. Load-time validation fails
loud on a blank `probe.tool` or one outside the server's `tools:` allowlist.

Exit codes: `0` all requested servers connected (and, with `--deep`, all
deep checks passed) · `1` at least one failed (or a requested name is
unknown/gated off) · `2` usage error.

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

- The plain probe is **point-in-time liveness + tool discovery**; it does
  not call tools. `--deep` closes most of that gap where servers advertise
  an auth-status tool or declare a `probe:` canary, but broader tool
  *behavior* (scopes, quotas, correctness of every tool) still belongs to an
  eval (`docs/EVALS.md`) or a manual tool call — a green canary proves one
  declared call works, not all of them.
- HTTP catalog servers are probed with the manifest's resolved headers/TLS;
  per-user hosted connectors (Settings → Connections) are NOT covered here —
  they get their own add-time validation handshake
  (`docs/CONNECTOR-ONBOARDING.md`).

## The rest of the testing ladder

| Layer | Command / surface | What it proves |
| --- | --- | --- |
| Manifest shape | `fleet validate-config` | Bundle/manifest well-formed, env keys named, executables present |
| Server liveness | `fleet mcp test --all` | Each server spawns, handshakes, lists tools with real credentials |
| Credential validity | `fleet mcp test --all --deep` | Each server's auth-status tool confirms the upstream accepts the credentials |
| End-to-end data path | `fleet mcp test --all --deep` + manifest `probe:` | Each server's declared read-only canary call returns real data from the upstream |
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
