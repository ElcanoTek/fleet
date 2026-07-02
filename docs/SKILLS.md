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
skills the operator already shipped. A skill's `allowed-tools` frontmatter is
parsed but **not** enforced as a hard authorization boundary — the real
boundaries remain the sandbox, the MCP tool allowlists, and the critical-tool
audit gate.

## Deferred (phases 2/3)

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
