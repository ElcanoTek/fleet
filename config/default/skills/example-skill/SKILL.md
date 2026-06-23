---
name: example-skill
description: Annotated template that documents this bundle's Agent Skills format — what a SKILL.md is, the three progressive-disclosure levels, and how to bundle a script. Read it when you want to author a new skill; it is a reference, not a real capability to invoke.
---

# Example skill (annotated template)

This is the generic bundle's one example skill. It exists to show the **shape** of
an Agent Skill so you can fork it. Copy this folder, rename it, rewrite the
frontmatter and body, and you have a new skill. (Compare `protocols/example.md`,
the equivalent template for protocols.)

A **skill** is a self-contained folder under the bundle's `skills/` directory that
follows the [Agent Skills standard](https://github.com/anthropics/skills):

```
skills/
  <skill-name>/
    SKILL.md          # this file: YAML frontmatter + instructions
    scripts/          # OPTIONAL: code the agent runs via bash / run_python
      hello.py
    REFERENCE.md      # OPTIONAL: reference material the agent reads on demand
```

## Frontmatter (required)

Every `SKILL.md` opens with a YAML frontmatter block fenced by `---`:

- `name` — must match the folder name; lowercase letters, digits, and hyphens;
  max 64 characters.
- `description` — what the skill does **and when to use it**. This is the only
  text fleet shows in the system-prompt roster, so make it specific — a vague
  description means the skill never gets picked up.

Additional standard fields (`allowed-tools`, `license`, `metadata`, …) are
accepted and ignored by fleet's loader. Note: fleet does **not** yet enforce
`allowed-tools` as a hard authorization boundary — the real boundaries are the
sandbox, the MCP tool allowlists, and the critical-tool audit gate.

## How fleet loads a skill (progressive disclosure)

fleet loads skills in three levels, so installing many skills costs almost no
context until one is actually used:

1. **Level 1 — metadata (always in the prompt).** Only this skill's `name`,
   `description`, and path appear in the system prompt.
2. **Level 2 — instructions (on demand).** When a task matches the description,
   read this `SKILL.md` in full (e.g. `view_file skills/example-skill/SKILL.md`).
3. **Level 3 — resources & scripts (as needed).** Bundled files are read or run
   only when the instructions call for them. Running a script returns just its
   output to the context, not its source.

The `skills/` directory is bind-mounted read-only into the sandbox and symlinked
into your workspace, so relative paths like `skills/example-skill/scripts/hello.py`
resolve for both `bash` and `run_python`.

## Worked example — run the bundled script

This skill ships a tiny script at `scripts/hello.py`. To demonstrate Level 3,
run it from your workspace:

```bash
python3 skills/example-skill/scripts/hello.py "fleet"
```

It prints a greeting. Replace it with a real, deterministic operation — a data
transform, a validator, a report generator — that is more reliable run as code
than re-derived by the model each turn.

## Writing your own

1. Copy this folder to `skills/<your-skill-name>/`.
2. Rewrite the frontmatter `name` (match the new folder) and `description`.
3. Replace this body with concrete, step-by-step instructions.
4. Add scripts under `scripts/` and reference files (e.g. `REFERENCE.md`) only as
   needed, and tell the agent in the body when to read or run each.
5. Keep skills you trust: a skill can run code in the sandbox, so review bundled
   scripts the way you review the bundle's `mcp/` servers.
