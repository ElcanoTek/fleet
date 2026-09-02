# Agent Plugins (#1166)

fleet loads **Agent Plugins** — the open, vendor-neutral package format from
[agent-plugins.org](https://agent-plugins.org) (specification **v1.0.0**) — so a
plugin written once for Cursor, VS Code, Copilot, Codex, Kiro or any other
compatible client drops into a fleet bundle unchanged, and a fleet bundle's own
skills + MCP servers can be packaged for those clients the same way.

An Agent Plugin is a directory with one required manifest and optional
components in fixed locations:

```
my-plugin/
├── plugin.json              # REQUIRED: "$schema" + "name" (+ metadata)
├── skills/
│   └── summarize/
│       ├── SKILL.md         # Agent Skills, exactly what skills/ already holds
│       ├── scripts/
│       └── references/
├── mcp.json                 # stdio / streamable-http (/ legacy sse) MCP servers
└── com.example.client/      # client-owned extension dirs — fleet ignores them
```

This page is the design note for what shipped, how the portable format maps
onto fleet's existing skills + MCP model, and what was deliberately left out.
The invariant-level reasoning is [ADR-0054](adr/0054-agent-plugins.md).

## Where fleet looks

Two places, both bundle content (`FLEET_CLIENT_CONFIG_DIR`):

1. **`<bundle>/plugins/`** — every immediate child directory is a plugin
   candidate. Files beside the plugin directories (a README) are ignored; a
   directory with no `plugin.json` is reported as "not a plugin" and skipped.
2. **`plugin_roots:`** in `manifest.yaml` — extra directories whose immediate
   children are plugins. Absolute, or relative to the bundle root:

   ```yaml
   plugin_roots:
     - vendor/plugins           # bundle-relative
     - /opt/fleet/site-plugins  # absolute
   ```

   A configured root that does not exist is reported (the fixed `plugins/` dir
   is optional and silent when absent).

A plugin is **bundle content**: it ships in the same operator-owned checkout
as `mcp/` and `skills/`, and it inherits exactly the bundle's trust class. A
plugin's stdio server runs host-side under the credential broker the way a
manifest `mcp_servers[]` entry does; its skills are files the agent reads inside
the sandbox. Nothing in this feature adds a new host-side exception (ADR-0036's
list is unchanged) — the loader only *translates* the portable format into the
two bundle primitives fleet already governs.

## How the format maps onto fleet

| Agent Plugins | fleet |
| --- | --- |
| `plugin.json` `name` | The plugin's identity in `fleet validate-config`, the skills library badge, and the `PLUGIN_DATA` directory name. |
| `skills/<name>/SKILL.md` | Parsed by the same `clientconfig.ReadSkills` as the bundle's `skills/`, then **materialized into the merged skills tree** between the built-in pack and the bundle's own skills. Downstream (prompt roster, sandbox mount, `/skills` API, `/name` invocation) needs no seam change: the roster handle is `skills/<name>/SKILL.md` as for every skill. |
| `mcp.json` `stdio` entry | One always-on `ServerDef` of type `stdio`, appended to the MCP catalog. The subprocess **launches in the plugin root** (or the declared `cwd`), with `PLUGIN_ROOT` and `PLUGIN_DATA` set *last* in its environment. |
| `mcp.json` `streamable-http` entry | One always-on `ServerDef` of type `http` with the literal URL and headers. |
| `mcp.json` `sse` entry | **Reported and skipped.** fleet speaks stdio and Streamable HTTP; the spec makes legacy HTTP+SSE support optional. |
| `extensions` / `com.example.client/` | Ignored, as the spec requires for namespaces a client does not implement. fleet reserves **`com.elcanotek.fleet`** and reads nothing from it yet (see below). |

### Skills: precedence and provenance

The merged skills tree (`builtin_skills.go`) is built lowest-precedence first,
so a later copy overwrites an earlier one:

1. fleet's **built-in pack** (unless `skills_builtin: false`; minus `skills_hidden`)
2. each **plugin's** skills — between two plugins claiming one name, the **first
   by plugin name wins** and the loser is logged
3. the bundle's **own `skills/`** — the bundle author always wins

`GET /skills` reports each row's provenance as `source: "bundle" | "plugin" |
"builtin"`, with `plugin: "<name>"` when it came from a plugin, and the skills
library badges it ("Plugin: acme-tools"). A bundle that opts out of the built-in
pack still gets a merged tree when a plugin ships skills — the tree is the only
way plugin skills reach the one `skills/` mount.

Editing a plugin skill's body in place is picked up on the next read (the same
live-reload contract as bundle skills). *Adding* a skill folder to a plugin, or
adding a plugin, needs a bundle reload (`fleet mcp reload`, SIGHUP, or a
restart) because the loader validates the folder set at load time.

### MCP servers: what the loader enforces

Per spec §7.2, validated in two stages. The **top level** of `mcp.json` must be
a JSON object with exactly `$schema` (the 1.0.0 identifier, matching
`plugin.json`'s version) and `mcpServers`; any failure **disables MCP for that
plugin only** — its skills still load. Then **each server entry** is validated
independently against its declared `type`'s closed field set; a failure skips
**that entry only**:

- **Server key**: 1–64 chars of `[A-Za-z0-9_-]`. The agent addresses tools as
  `mcp_<server>_<tool>`, so a key with a dot or a space would produce a tool
  name providers reject. The key is used **as declared** — no namespacing — so
  `fleet mcp test <key>` and the connections UI show the author's name. A key
  that collides with a manifest `mcp_servers[]` / `http_tools[]` name, the
  reserved `_http`, or an earlier plugin's server is skipped (manifest first,
  then plugins in name order).
- **`stdio`** (`type`, `command`, `args`, `env`, `cwd`): `command` is one
  executable token — a bare name (PATH lookup at spawn, as for any manifest
  server) or a `./`-relative path resolved under the plugin root and required
  to exist there after symlink resolution. Absolute, `../`, and
  placeholder-bearing commands are rejected. `${PLUGIN_ROOT}` / `${PLUGIN_DATA}`
  are expanded in `args`, `env` values and `cwd` — one textual pass, never
  rescanning what it inserted, never in `command`, env keys, URLs or headers;
  any other `${…}` text stays literal. `env` must not set the two reserved
  names. `cwd` must be `./`-relative, `${PLUGIN_ROOT}`-rooted or
  `${PLUGIN_DATA}`-rooted and must stay inside the corresponding directory
  (a `PLUGIN_DATA` cwd is created before launch).
- **`streamable-http`** (`type`, `url`, `headers`): absolute http(s) URL, no
  user info, no fragment, `https` unless the host is `localhost` or a loopback
  IP. Header names are RFC 7230 tokens, unique case-insensitively, values
  without line breaks. Headers are **visible package data** — the spec forbids
  secrets in them, and fleet applies no `${ENV}` interpolation to them.

**`PLUGIN_DATA`** is `<state>/plugin-data/<plugin-name>` under the fleet data
dir (`FLEET_DATA_DIR`, then `CHAT_DATA_DIR`, then the user cache — the same
trusted base as the merged skills tree, never a shared `/tmp` path). It is
created before any stdio server launches, is writable by the process the
broker runs servers as, and lives outside the plugin root so it survives a
plugin update.

**Gates unchanged.** A plugin server is `always: true` — the portable format
has no credential gate because it must not carry secrets, so there is nothing
to gate on — and from there it flows through every gate a manifest server does:
the child-side scope authorizer (ADR-0042) re-derives the server set from the
bundle it loads itself; `agent_policy.critical_tools` suffixes hold its writes
for approval; the connections page lists it in the always-on
visible-but-locked rows; `fleet mcp reload` / SIGHUP re-read it with the rest
of the catalog.

### Failure boundaries (spec §4–7, exactly)

| Defect | Effect |
| --- | --- |
| Unknown top-level field in `plugin.json` | **Reported and ignored**; the plugin loads. |
| `extensions` is not an object | Reported and ignored; components load. |
| Missing/invalid `$schema` or `name`, wrong field type, unknown `author` member, non-object extension value, invalid JSON | **Plugin rejected**: nothing of it is discovered or executed. |
| `plugin.json` resolves outside the plugin root | Plugin rejected. |
| `skills` exists but is not a directory, or escapes the root | Skills disabled for that plugin; MCP still loads. |
| One skill malformed (bad frontmatter, name ≠ folder) or its `SKILL.md` escapes the root | That skill skipped; siblings load. Nested skill dirs are never searched. |
| A file inside a skill folder resolves outside the plugin root | That file is not copied into the merged tree (logged); the skill still loads. |
| `mcp.json` invalid JSON / unknown top-level field / wrong or mismatched `$schema` / missing `mcpServers` | MCP disabled for that plugin; skills still load. |
| One server entry invalid (unknown field, bad type, escaping command or cwd, reserved env key, non-loopback `http`, bad header) or transport `sse` | That entry skipped; siblings load. |
| Two plugins declare the same `name` | The second (by root order, then directory name) is skipped; reported. |

Every problem is logged at load as `clientconfig: warning: plugin: …` and
surfaced by `fleet validate-config` as a **non-blocking advisory** on the
`manifest` check, whose OK detail also counts the plugins loaded.

## What was deliberately not done

- **No fleet-specific extension data is read.** `com.elcanotek.fleet` is
  reserved (a constant in `plugins.go`) so a future fleet knob — a per-server
  `tools:` allowlist, a `probe:` declaration, a credential gate — has a stable,
  non-portable home. Nothing is parsed from it yet; a plugin that carries it
  loads exactly as one that does not.
- **No `tools:` allowlist and no `probe:` for plugin servers.** The portable
  format has no field for either. Govern a plugin server's tools through
  `agent_policy` (critical-tool suffixes) as for any server; `fleet mcp test`
  covers the handshake + `tools/list` rung and reports the server as
  unproven-beyond-handshake, never failed.
- **Fleet's own spawn-time tokens are still substituted.** A plugin env value
  containing `${FLEET_WORKSPACE}` or `${FLEET_TASK_ID}` is rewritten at spawn
  exactly like a manifest server's (docs/MCP-BUNDLE-ENV.md). The spec says a
  client "MUST NOT perform any other placeholder or environment-variable
  expansion"; this is a documented, narrow deviation rather than a second
  substitution path, and a portable plugin has no reason to write those tokens.
- **Kubernetes backend caveat, inherited.** Plugin skills live in the merged
  skills tree, which the kubernetes sandbox backend cannot mount (the tree is
  under the control plane's data dir, not in any sandbox image — see
  `IsMaterializedSkillsDir`). On that backend a plugin's skills are in the
  prompt roster and the `/skills` API, but not readable *inside* the sandbox —
  the same limitation the built-in pack already has, and the bundle's
  `skills_builtin: false` does not lift it for plugins.
- **No marketplace, installer, signing or update checker.** Distribution and
  installation are out of the portable contract by design; a plugin arrives by
  being committed to the bundle checkout and reviewed like any other bundle
  content. `version` is displayed, not compared.
- **Scheduled runs**: like every bundle skill, plugin skills are rostered for
  interactive chat only (`internal/scheduledrun` emits no bundle-skill roster;
  see docs/SKILLS.md). Plugin MCP servers are in the catalog for both paths.

## Authoring one

The example lives in
[`ElcanoTek/example-config` → `plugins/example-plugin/`](https://github.com/ElcanoTek/example-config/tree/main/plugins/example-plugin):
a `plugin.json`, one skill, and an `mcp.json` whose stdio server exercises
`${PLUGIN_ROOT}` / `${PLUGIN_DATA}`. Minimal manifest:

```json
{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "my-plugin"
}
```

`name` is 1–64 chars of `a-z 0-9 . -`, alphanumeric at both ends, no `--` or
`..`. Skills follow the Agent Skills rules fleet already enforces (frontmatter
`name` equals the folder, non-empty `description`). Check the result with
`fleet validate-config` — plugin problems appear on the `manifest` line — and
`fleet mcp test <server>` for a plugin server's handshake.

## Enforcement

`internal/clientconfig/plugins.go` is the loader; `plugins_test.go` covers
every row of the failure-boundary table above through `clientconfig.Load`
(manifest deviations, MCP two-stage validation, cwd forms, name collisions,
skill precedence and provenance, symlink escapes, `plugin_roots`, duplicate
names, single-pass expansion). The httpapi and web tests cover the `plugin`
provenance on `/skills`. `internal/evals/fingerprint.go` hashes `plugins/` as
bundle content so eval runs against different plugin sets are not compared.
