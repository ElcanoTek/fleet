# Skills (#513, phase 1)

fleet runs [Agent Skills](https://github.com/anthropics/skills): self-contained
folders under the client bundle's `skills/` dir, each a `SKILL.md` (YAML
frontmatter `name` + `description`, then instructions) that may bundle scripts
and reference files. Skills are **bundle-owned** — they ship in the operator's
client-config bundle (`FLEET_CLIENT_CONFIG_DIR`), are validated at load
(`internal/clientconfig/skills.go`), and are bind-mounted **read-only** into the
per-turn sandbox, so a skill's bundled scripts run inside the same governed
sandbox as everything else. fleet has no bespoke skill executor: skills are just
files the agent reads and runs with `bash`/`run_python`.

## Three-tier progressive disclosure

1. **Level 1 — roster.** Only each skill's name + description + path go in the
   system prompt (`internal/agent/prompt.go`), so a big skill library costs a
   few lines of context.
2. **Level 2 — instructions.** The agent reads `skills/<name>/SKILL.md` on
   demand when a task matches (or when explicitly invoked, below).
3. **Level 3 — resources.** Bundled scripts / reference files under
   `skills/<name>/` are read or executed on demand, in the sandbox.

## Browse: `GET /skills`

The chat server exposes the roster (auth + member gated, like `/personas`):

```json
{ "skills": [ { "name": "example-skill", "description": "…" } ] }
```

Nil-safe: a server without a bundle (or a bundle without a `skills/` dir)
returns `{"skills":[]}`. The web app proxies it at `/api/skills` and fetches it
once at startup to drive the composer autocomplete. Like personas/protocols,
the roster is re-read from disk per request, so an operator editing a skill in
place is picked up without a restart.

## Explicit invocation: `/skill-name`

Start a chat message with `/<skill-name>` to invoke a skill explicitly instead
of relying on relevance. The rule is deterministic and strict
(`matchSkillInvocation` in `internal/httpapi/skills.go`):

- The `/` must be the **first character** of the message.
- The token runs to the first whitespace (or end of message) and is compared
  **exactly, case-sensitively** against the bundle's skill names. Arguments
  after the token are fine: `/research-report on topic X` invokes
  `research-report`.
- An unknown `/token` gets **no block and no error** — a leading slash is
  common in normal text (paths like `/etc/hosts`, whose token `etc/hosts` can
  never be a skill name), so only exact skill-name matches trigger. The message
  sends as plain text.

A matched invocation appends a block to the user message before the run:

```
[Skill invoked: <name>]
The user explicitly invoked the skill "<name>". Read `skills/<name>/SKILL.md`
now and follow its instructions for this request; …
```

**Run-log honesty:** the block is appended to the *persisted* user message, so
the conversation transcript itself records which skill was invoked and what the
agent was told to do — that is how "show which skill loaded in the run" is
satisfied. There is no separate skill-invocation event or log stream, and no
guarantee beyond the instruction itself that the model actually reads the
SKILL.md (it reliably does, but it is a prompt-level contract, not an enforced
one).

In the web composer, typing `/` at the start of the message opens a small
autocomplete popover over the bundle roster (prefix filter, ↑/↓ + Enter/Tab to
complete to `/name `, Esc to dismiss). The popover is a typing aid only — the
server is the authority on what matches.

## Security posture (unchanged)

A skill can ship code that executes in the sandbox; the bundle is a
trusted-but-reviewable supply chain, and a skill is only as trustworthy as the
bundle it ships in. Explicit invocation does not change that: it selects among
skills the operator already shipped.

A skill's `allowed-tools` frontmatter is parsed and **surfaced** — the skills
library UI and `GET /skills` show a skill's declared tools next to it, so an
operator reviewing the supply chain can see the contract each skill claims —
but it is **not** enforced as an authorization boundary. The real boundaries
remain the sandbox, the MCP tool allowlists, and the critical-tool approval
gate. This is deliberate, not a gap:

- **There is no "active skill" to gate on.** Skills are read on-demand
  mid-turn — the model reads a `SKILL.md` by path when it seems relevant, may
  read several, and may follow one only partially. There is no
  activate/deactivate event, so "which skill's `allowed-tools` applies to this
  tool call?" has no honest answer. (Contrast personas, which are selected once
  per turn and therefore *can* carry a hard per-turn tool allowlist.)
- **A skill can never exceed the turn's existing capabilities.** Enforcing
  `allowed-tools` could only ever *narrow* an already-authorized set, so it
  can't stop anything the sandbox / MCP allowlist / approval gate don't already
  stop.
- **The list is author-controlled.** It is written by the same party you would
  need to defend against, so treating a self-declared list as a security
  boundary would be theater. Surfacing it for human review is the honest use.

**Portability note:** skills imported from Claude Code declare tools by *its*
names (`Read`, `Grep`, `Bash`), which differ from fleet's (`read_file`,
`bash`, `run_python`). fleet surfaces the declared names verbatim and does not
lint them against its own tool namespace — an imported skill's `allowed-tools`
is informational here regardless of naming.

If enforceable least-privilege for *trusted* skills is ever required, the clean
design is a separate explicit "run this one skill" mode with a real
platform-set active skill (where `effective_tools = baseline ∩ allowed-tools`),
not a gate retrofitted onto ambient read-on-demand skills — and that would
warrant its own ADR.

## Built-in skills pack + library UI

Fleet embeds seven generally-useful skills in the binary
(`internal/clientconfig/builtin_skills/`): `data-profiler` (stdlib-only
profiler script), `bento-slides` (vendored single-file Bento deck app, a
document splice helper, and a browserless PDF export —
[BENTO-PDF-EXPORT.md](BENTO-PDF-EXPORT.md)), `browserbase` (hosted-browser
session handoff, paired with the Browserbase connector and the
`browserbase_live_view` tool — [BROWSERBASE.md](BROWSERBASE.md)),
`web-research-brief`, `code-review-checklist`,
`release-notes`, `executive-report`. Every bundle inherits them by default —
the skills analogue of the built-in MCP directory.

Skills are real files the sandbox bind-mounts, so inheritance works by
**materialization**: `clientconfig.Load` syncs the bundle's `skills/` and the
embedded pack into a merged on-disk dir (under `$FLEET_DATA_DIR/skills-merged/`,
keyed by bundle path, never world-writable `/tmp`; bundle wins a name
collision, loudly) and points
`Bundle.SkillsDir` at it. Every consumer — prompt roster, sandbox mounts,
workspace symlinks, `/skills`, taskrun (mount only, see below), evals — picks
the pack up unchanged.
`Skills()` resyncs from sources on read, preserving the edit-a-skill-in-place
live-reload contract; `ValidateSkills` runs against the bundle's own dir
(`Bundle.BundleSkillsDir`). Workspace symlinks honor the registered dirs
(`tools.SetSupportingDocDirs`, wired at boot) rather than only the legacy
`$CWD/<name>` convention.

**Where the roster is actually surfaced.** Bundle and built-in skills are
discoverable in **interactive chat turns** and in eval replays, which build their
prompt through `internal/agent/prompt.go` (section 5b). Scheduled runs and
`fleet task run` compose their prompt in `internal/scheduledrun` instead, which
emits the owner's *user-authored* skills but **no bundle-skill roster** — so a
scheduled task will not discover a built-in skill on its own. The merged dir is
still bind-mounted into those runs, but the relative `skills/<name>/…` path does
not resolve there either: the workspace `skills` symlink is planted by
`EnsureWorkspaceDir` (per-conversation) and a forced working dir has no
supporting-doc read exception. Treat bundle skills as an interactive-chat
capability; a scheduled task that needs one should inline the instructions in its
prompt.

**On the kubernetes sandbox backend the same tree is staged into the workspace
claim.** A sandbox pod mounts only the workspace claim, and the merged tree
under the control plane's data dir is something no pod mounts and no sandbox
image can carry (its name is derived from the bundle path, and it is rebuilt
at boot from the binary's pack, the plugins and the bundle). So at boot fleet
re-materializes the complete tree — pack, plugin skills, bundle `skills/` —
at `<workspace root>/skills` inside the claim and points `SkillsDir` there;
every pod mounts that directory read-only (a `subPath` of the claim, like the
shared file library), the fileop anchor resolves reads under it and refuses
writes, and `Skills()` resyncs it on read, so `skills/<name>/SKILL.md`
resolves for the file tools, bash and python exactly as on podman. No image
bake and no `bundle_docs_in_image` declaration is involved for skills;
`skills_builtin: false` is a taste decision on both backends. Design and the
tamper-resistance argument:
[ADR-0055](adr/0055-kubernetes-skills-staged-into-the-workspace-claim.md);
operator view:
[DEPLOYMENT-KUBERNETES.md](DEPLOYMENT-KUBERNETES.md#skills-inside-a-sandbox-pod).

Manifest knobs (mirroring the MCP directory):

```yaml
skills_builtin: false      # opt out of the built-in pack entirely
skills_hidden: [name, …]   # drop individual built-in skills
```

**Library UI**: Settings → Skills (account menu, next to Connections; visible
from both the chat and Operations Center shells) — searchable roster with
Workspace/Built-in provenance badges and a full SKILL.md read view backed by
`GET /skills/{name}` (names resolve against the loaded roster, never raw
paths).

## Skills from Agent Plugins (#1166)

A skill can also arrive inside an [Agent Plugin](AGENT-PLUGINS.md) — the
portable `plugin.json` + `skills/` + `mcp.json` package under the bundle's
`plugins/` dir. Plugin skills are parsed by the same `ReadSkills` and land in
the same merged tree, so everything on this page applies to them unchanged: the
roster handle is `skills/<name>/SKILL.md`, `/name` invocation works, and
`allowed-tools` is surfaced, never enforced. Precedence is bundle `skills/` >
plugin (first by plugin name) > built-in pack; `GET /skills` reports
`source: "plugin"` plus `plugin: "<name>"` so the library can badge provenance.

## User-authored skills (the builder — phase 2, shipped)

Settings → Skills gains a **"Your skills"** builder: create, edit,
enable/disable, and delete personal skills (name + one-line description +
markdown instructions). They are DB-owned (`user_skills`, migration 033),
strictly per-user, and reach runs by **workspace materialization**: before a
chat turn, the caller's ACTIVE skills are written into the conversation
workspace (`user-skills/<name>/SKILL.md`, regenerated from the DB fields and
cleaned up when a skill is renamed/disabled/deleted) and listed in a "Your
user's skills" prompt roster section. `/name` invocation matches them after
bundle/built-in names.

**Scheduled tasks load them too**: the runner resolves the task owner
(`task.CreatedBy` → chat email, the same resolution the remote-MCP overlay
uses) and inlines the owner's ACTIVE skills into the run's system prompt
(full bodies, under a 24KB total budget with LOUD truncation — headless runs
have no per-conversation workspace to materialize files into, and per-user
files in the shared workspace root would be readable by other users' runs).

Remaining scope notes:

- No bundled scripts on user skills (SKILL.md only); anything executable a
  user skill describes still runs under the ordinary sandbox + tool policy.
- Graduation to the whole deployment stays an operator action: copy the
  SKILL.md into the bundle's `skills/` dir (the read view makes the content
  copyable).

## Agent-drafted proposals (phase 3, shipped)

The `propose_skill` native tool ("save from run") is registered in BOTH modes
under the same lockstep rule as `propose_note`: the tool exists iff a
`SkillProposer` is wired, and the call is intercepted at the policy boundary
(`checkSkillProposal`) — the tool body never executes; the staging IS the
effect. A proposal lands as a `proposed` builder skill for the OWNER
(interactive: the turn's user; scheduled: the task owner), completely inert —
never materialized, never invocable, never inlined — until the owner approves
it on the Skills page ("Proposed by agent" badge → Approve, or Delete to
reject). Nothing can move a skill back to `proposed`; only the agent path
creates that state. Same trust posture as memory/note proposals: the agent
suggests, the human decides.

## Original phase plan (phases 2/3 above are now shipped; kept for history)

Per the maintainer's phasing comment on #513:

- **Phase 2 — user-authored / project-scoped skills.** Create/edit skills
  in-app, stored as **DB-staged proposals** (or a staged artifact area), *not*
  written directly into the operator-owned client bundle. Ties into project
  scoping (Projects/Spaces, #509).
- **Phase 3 — "save from run".** Capturing a run as a proposed skill with
  diff/review/**approval**, then optional **export to a bundle repo by an
  operator**. Agent-authored skill writes require approval by default in
  enterprise mode and should be eval-gated before becoming active for
  scheduled tasks.

Phase 1 (this document) is read-only over the bundle: browse + explicit
invocation + transcript-visible loading. There is no write path of any kind.
