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
| `extensions["com.elcanotek.fleet"]` | fleet's own namespace (spec §8.1): per-server `tools`, `probe`, and the Optional-server knobs the portable format has no field for. See [Fleet's extension namespace](#fleets-extension-namespace). |
| other `extensions` / `com.example.client/` | Ignored, as the spec requires for namespaces a client does not implement. |

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

Plugin skills follow the disk on every read, exactly like bundle skills: editing
a body, adding a skill folder, or adding a `skills/` dir to a plugin that had
none is picked up on the next `Skills()` read with no restart. Adding a whole
*plugin*, or changing its `mcp.json`, needs a bundle reload (`fleet mcp reload`,
SIGHUP, or a restart) — the same rule as a manifest `mcp_servers[]` change.

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

### Fleet's extension namespace

The portable format deliberately has no field for a client's governance knobs,
and the spec's answer is a reverse-domain namespace in `plugin.json`'s
`extensions` object (§8.1) that every other client ignores. fleet owns
**`com.elcanotek.fleet`** and reads one thing from it: per-server overrides keyed
by the server's `mcp.json` name.

```json
{
  "$schema": "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json",
  "name": "acme-tools",
  "extensions": {
    "com.elcanotek.fleet": {
      "mcp_servers": {
        "validator": {
          "tools": ["validate", "explain"],
          "probe": {"tool": "validate", "contains": "ok", "args": {}},
          "optional": true,
          "enabled_by_default": false,
          "beta": false,
          "display_name": "Acme validator",
          "description": "Validates release manifests.",
          "data_sources": ["https://api.acme.example"],
          "disabled": false
        }
      }
    }
  }
}
```

| Key | Maps to | Notes |
| --- | --- | --- |
| `tools` | `ServerDef.Tools` — the per-server **allowlist** the ADR-0042 child authorizer enforces | Trimmed, de-duplicated; empty = every advertised tool, as for a manifest server. |
| `probe` | `ServerDef.Probe` — the `fleet mcp test --deep` canary | `tool` required; `contains` and `args` optional. Must be inside `tools` when an allowlist is set (the manifest rule), else the probe is dropped with a report. |
| `optional`, `enabled_by_default`, `beta`, `display_name`, `description`, `data_sources` | the same-named `ServerDef` fields | The Optional-server semantics and settings-UI metadata a manifest server has. |
| `disabled` | — | `true` drops the entry without editing `mcp.json` — how an operator vendoring a third-party plugin turns off a server they don't want. Not reported (it is an explicit choice). |

Deliberately **not** expressible here: anything credential-shaped (`env`,
`enabled_env`, `account_vars`, `identity_env`). The portable format forbids
secrets in a plugin and fleet does not smuggle them in through its own
namespace; a server that needs a brokered credential belongs in the manifest's
`mcp_servers[]`.

Because this is fleet's namespace, its failure handling is fleet's to choose,
and it is lenient to match the spec's top level: an unknown key is reported and
ignored; a wrong-typed override is reported and ignored **for that server
only**; an override naming a server `mcp.json` does not declare is reported;
nothing in the extension can reject the plugin. (A non-object *value* for the
namespace is still fatal — that is the portable schema's rule, not fleet's.)
fleet reads no extension *directory* (`com.elcanotek.fleet/` at the plugin
root).

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

- **No credential gate for plugin servers.** The extension namespace carries
  `tools`, `probe` and the Optional-server knobs, but nothing credential-shaped:
  a plugin server that needs a brokered secret is a manifest `mcp_servers[]`
  entry, not a plugin entry.
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
names, single-pass expansion), the `com.elcanotek.fleet` overrides (allowlist
+ probe applied, probe-outside-allowlist dropped, `disabled`, wrong-typed and
unknown keys reported per server, undeclared server names reported), and
skill folders added or removed after load appearing on the next read. The httpapi and web tests cover the `plugin`
provenance on `/skills`. `internal/evals/fingerprint.go` hashes `plugins/` as
bundle content so eval runs against different plugin sets are not compared.
