# Tools hidden from interactive chat

`agent_policy.interactive_hidden_tools` lets a bundle keep MCP tools out of
interactive chat while scheduled runs keep them. The *why* behind the shape is
[ADR-0085](adr/0085-interactive-hidden-tools.md).

## Why

The only way to hide an MCP tool used to be the server's `tools:` allowlist
(Gate-2), and that applies to every run. A deal-booking bundle needs the
opposite split. Its scheduled multi-deal-update protocol calls the raw SSP
write tools (`ox_create_deal`, `ix_merge_deal_domains`, …) directly. In chat
those same writes must go through the bundle's plan flow
(`*_preview_plan` → one approval card → `*_execute_plan`). A real chat log
showed the model calling a raw write through the generic `tool_call` bridge
instead, which staged one approval card per deal.

## The key

```yaml
agent_policy:
  interactive_hidden_tools: [ox_create_deal, ix_merge_deal_domains]
```

Members are tool-name suffixes, matched the way `critical_tools` is: a member
selects `mcp_<server>_<tool>` when the name equals it or ends with
`_<member>`. So `create_deal` hides every server's `create_deal`, and
`mcp_openx_ox_create_deal` hides exactly that one. Only MCP tools are hidden
(bundle servers, `http_tools`, `a2a_peers` and a user's hosted connectors);
native tools such as `bash` are not. With the key absent, every run sees the
same tools as before. The key changes nothing for a turn's `RunConfig` unless
it is set.

## What it does in a chat turn

The interactive driver reads the installed list at the start of each turn
(`agentcore.InteractiveHiddenTools()`) and passes it to `agentcore.Run` as
`RunConfig.HiddenToolSuffixes`. All the enforcement reads that one list:

| Path the model could use | What happens |
| --- | --- |
| The tool list sent to the model | The tool is not in it. It is dropped in `buildFantasyToolsWithRoster`, after Gates 1 and 2 and before the persona policy and the disclosure decision. |
| `tool_search` / `tool_describe` | The tool never enters the deferred registry, so search does not find it and describe refuses it. |
| `tool_call {name: …}` | Refused with the explanation below. The broker is never reached. |
| A direct call to the hidden name | The out-of-roster recovery (#1744) records the gate as `interactive_hidden` and answers with the same explanation. The metric's `reason` label is `interactive_hidden`. |
| `spawn_subagent` from the chat turn | The child inherits the list at every depth (`buildChild`), so delegating cannot reach the tool. |
| Any other dispatch of a built MCP tool | `mcpTool.Run` checks the list first, before the policy gate. A hidden critical tool therefore never stages an approval card, nothing is journaled, and the broker is not called. |
| The approval-card describer (ADR-0082) | Not called when it is hidden. It runs on the chat turn's scope, so it must not reach what the model could not. The card falls back to the generic one (`gated`). |

The explanation is the same text everywhere:

> `mcp_openx_ox_create_deal is not available in interactive chat and was not
> called: this deployment reserves it for scheduled runs
> (agent_policy.interactive_hidden_tools). Do not look for another way to call
> it; use the workflow your instructions give for this action in chat.`

The system prompt's live-registry section is built from the roster, so it
never lists a hidden tool.

## What it does not change

- **Scheduled runs, Operations Center tasks and the A2A server.** They pass no
  list and see every tool their gates allow. A task scheduled *from* chat is a
  scheduled run, so it can use a hidden tool. That is the point: it is how the
  scheduled protocol reaches them. The chat can still schedule such a task,
  and the task then runs under the scheduled run's governance (audit gate,
  `confirm_audit`), not under a chat approval card. If that is not acceptable
  for a tool, gate scheduling in the bundle's persona or protocol.
- **Credentials and the broker.** The list is a parent-side narrowing like the
  persona policy (ADR-0042 keeps that one parent-side too). The
  credential-owning child still authorizes every call against its own bundle.
- **An approval card already staged.** A chat turn cannot stage a card for a
  hidden tool. A card staged before the key was added (only possible across a
  restart, since the policy is installed at boot) still runs if a person
  approves it: approving is a person's decision.

## Validation

At boot, a blank member or one with a character no MCP tool name can contain
(anything but ASCII letters, digits, `_`, `-`, `.`) is dropped and logged.
`fleet validate-config` fails `agent_policy` on the same members. It warns
(never fails) on a member that matches no tool the bundle declares, when it
can tell: every `mcp_servers` entry lists its `tools:` and there are no
`a2a_peers`, so the candidates are known. Such a member is most likely a
typo, but it may be meant for a per-user hosted connector's tool, which users
add at runtime and the bundle cannot list. The check is non-blocking, like
the rest of `agent_policy`.

## Code

- `internal/agentcore/interactive_hidden.go`: the list, its validation and
  the refusal text.
- `internal/agentcore/mcp_tools.go`: the registration drop and the call gate.
- `internal/agentcore/tool_disclosure.go`: the `tool_call` explanation.
- `internal/agentcore/out_of_roster.go`: the direct-call explanation.
- `internal/agent/manager.go`, `interactive.go`, `subagent.go`,
  `scheduled.go`: the turn's list and its inheritance by sub-agents.
- `internal/httpapi/approval_card.go`: the describer skip.
- `cmd/fleet/validate_config.go`: the preflight.

Tests: `internal/agentcore/interactive_hidden_test.go`,
`internal/agent/interactive_hidden_test.go`,
`cmd/fleet/validate_config_hidden_test.go`,
`internal/clientconfig/interactive_hidden_tools_test.go`, and
`TestStage_DescriberHiddenFromChatIsNotCalled`.
