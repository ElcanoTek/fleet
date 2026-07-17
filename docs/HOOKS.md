# Governed lifecycle hooks (#788)

A client bundle can declare **hooks** — commands that run at fixed points in an
agent run, inside the per-turn sandbox, to enforce organization checks, run a
formatter/validator, or observe lifecycle events. Hooks are a *governed* seam:
they can only observe or **narrow** authority, never widen it. See
[ADR-0038](adr/0038-governed-lifecycle-hooks.md) for the trust boundary.

## Schema

In the bundle `manifest.yaml`:

```yaml
hooks:
  version: 1
  entries:
    - id: block-secrets-in-bash        # unique, stable, shown in audit events
      event: pre_tool_use              # when it runs (see Events)
      matcher: "bash"                  # which tools (pre/post_tool_use only)
      command: "jq -e '.tool_input | test(\"AWS_SECRET\") | not'"
      timeout_seconds: 15              # default 30, clamped to [1,120]
      enforce: true                    # true = fail-closed; default advisory
    - id: gofmt-after-edit
      event: post_tool_use
      matcher: "edit_file"
      command: "/opt/hooks/gofmt-check.sh"
```

`matcher`: `""` or `*` = all tools; a trailing `*` is a prefix glob
(`mcp_*`); otherwise an exact tool name. It is only valid on `pre_tool_use` /
`post_tool_use`.

Adopting `hooks:` requires a fleet release that understands the schema — strict
YAML decoding rejects an unknown key on older binaries (additive-first). The
generic `config/default` bundle ships **zero** hooks. **Hooks install at
startup; changing them requires a restart** (the MCP hot-reload path does not
re-read them).

## Events

| Event | Fires | Outcomes honored |
|-------|-------|------------------|
| `user_prompt_submit` | after the ingress guardrail, before the first model call | block (fails the turn), additional_context (appended) |
| `pre_tool_use` | before a tool executes, before fleet's own policy gates | block (tool not executed), continue |
| `post_tool_use` | after a tool's output is governed, before it is recorded | additional_context (appended to the result) |
| `turn_end` | on normal turn completion | observational only (audited, not enforced) |

`turn_end` does **not** fire on a cancelled or budget-stopped run (the run
context is already dead, so a sandbox exec cannot run).

## The stdin/stdout contract

Each hook receives a single bounded JSON object on **stdin**:

```json
{"hook_api_version":1,"event":"pre_tool_use","mode":"scheduled","label":"...",
 "tool_name":"bash","tool_call_id":"...","tool_input":"...","tool_input_truncated":false}
```

All text fields are secret-redacted and byte-capped (tool input 8 KiB, result
preview 4 KiB). The payload never contains environment variables, credentials,
hidden reasoning, or a full transcript.

The hook prints its decision as a JSON object on **stdout** (the last parseable
JSON-object line wins, so diagnostics printed earlier are fine):

```json
{"decision":"continue","additional_context":"lint clean"}
{"decision":"block","reason":"policy X forbids this"}
```

- `reason` is capped at 1 KiB; `additional_context` at 4 KiB per fragment, with
  a 32 KiB per-run budget (over-budget fragments are dropped and audited).
- A hook that exits nonzero, times out, or prints no valid decision is a
  **failure**: an `enforce: true` hook **blocks**; an advisory hook is audited
  and the operation **continues**.

## Audit

Every invocation emits a `hook.decision` observer event (`hook_id`, `event`,
`tool_name`, `tool_call_id`, `decision`, `reason`, `duration_ms`,
`error_class`, `enforce`) — visible in the chat SSE stream and the scheduled
run's captain's-log, the same audit trail as every other governed decision.

## What hooks are NOT

Hooks are **not an authority-expansion mechanism**. A hook runs inside the
sandbox with no credentials and cannot add tools, network, filesystem roots,
budget, or approval authority. Fleet's existing host policy, approval,
credential, persona, and budget gates all evaluate **after** hooks on the
**same unmodified** tool input — so a hook can only refuse or annotate, never
approve one payload and execute another. Argument rewriting is deliberately not
supported.

## Deferred

`pre_compact`/`post_compact` and `subagent_start`/`subagent_end` events (child
runs already get `pre/post_tool_use` via the shared loop); argument rewriting
and an "ask" outcome; hook config hot-reload; `turn_end` on interrupted runs;
and multi-source hook discovery (fleet has one trusted source — the bundle).
