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
| The tool list sent to the model | The tool is not in it. It is dropped in `buildFantasyToolsWithRoster` before the disclosure decision, as the last gate: after the opt-in, allowlist, persona and schema gates. |
| `tool_search` / `tool_describe` | The tool never enters the deferred registry, so search does not find it. `tool_describe` answers with the explanation below, not "no tool named". |
| `tool_call {name: …}` | Refused with the explanation below. The broker is never reached. |
| A direct call to the hidden name | The out-of-roster recovery (#1744) records the gate as `interactive_hidden` and answers with the same explanation. The metric's `reason` label is `interactive_hidden`. |
| `spawn_subagent` from the chat turn | The child inherits the list at every depth (`buildChild`), so delegating cannot reach the tool. |
| Any other dispatch of a built MCP tool | `mcpTool.Run` checks the list first, before the policy gate. A hidden critical tool therefore never stages an approval card, nothing is journaled, and the broker is not called. |
| The approval-card describer (ADR-0082) | Not called when it is hidden. It runs on the chat turn's scope, so it must not reach what the model could not. The card falls back to the generic one (`gated`). |
| Email prevalidation (`validate_email_content`, when a chat turn stages `preview_email` / `send_email`) | Not called when it is hidden, for the same reason. Prevalidation is best-effort, so it is skipped exactly as when the validator is unreachable: the email is staged unvalidated and the skip is logged. |

The explanation is the same text everywhere:

> `mcp_openx_ox_create_deal is not available in interactive chat and was not
> called: this deployment reserves it for scheduled runs
> (agent_policy.interactive_hidden_tools). Do not look for another way to call
> it; use the workflow your instructions give for this action in chat.`

**Precedence.** A tool another gate also removes is reported under that gate,
not as hidden. The hidden list is the last gate because it is the narrowest:
the persona policy and the schema check remove a tool in every mode, so for a
tool the persona denies, "reserved for scheduled runs" would be false. Every
enforcement point gives the same answer:

- **The out-of-roster answer** reads the first gate recorded (`roster.removed`).
- **`tool_describe` and `tool_call`** keep the persona-denied and hidden names
  of the run apart. A persona-denied name gets the persona explanation the
  out-of-roster answer gives, and the hidden set holds only tools no earlier
  gate removed.

So a tool both gates remove is explained as the persona's, everywhere.

Because the hidden list is last, a chat turn now runs the schema check on a
hidden tool, and emits `persona_tool_blocked` for one the persona also denies.
That is the same work and the same events a scheduled run already produces
for that tool.

**What the per-turn prompt says.** The model is not told about a hidden tool
anywhere in a chat turn:

- The system prompt's live-registry section is built from the gated roster, so
  it never names or counts a hidden tool
  (`TestInteractiveHidden_LiveRegistryOmitsHiddenTools`).
- The chat prompt's tool-gated sections read `activeMCPToolNames`, which
  subtracts the hidden list. Today that is the `protocols/fastio-mcp.md`
  entry: it is listed while at least one fast.io tool is visible to chat, and
  left out when every one is hidden. `RunTurn` takes ONE snapshot of the list
  per turn and hands the same slice to the prompt and to the run gate, so a
  policy reload mid-turn cannot split them.
- The per-turn "Possibly-relevant connectors" note (#512) does not recommend a
  connector whose every tool is hidden, because connecting it would give the
  chat nothing. The picker does not list such a connector either. Both use
  one visibility rule (`chatVisibleTools`). A partly hidden connector is
  still recommended: what remains is usually the chat flow itself (a plan
  connector's `preview_plan` / `execute_plan` beside its hidden raw writes).
- Text a bundle writes in its persona or protocols is the bundle's to keep
  consistent with its own list.

## What the chat Tools picker shows

The picker (`GET /mcp-servers` before a conversation exists,
`GET /conversations/{id}/mcp-servers` after) lists what a chat turn can call,
so a trader is never shown a tool chat will refuse:

- A hidden tool is dropped from its connector's `tools` and `tool_count`, so
  it is not in the row's count, the library's tool list or the picker's search
  (no "↳ ox_create_deal" hit). An always-on row sends only a count; its hidden
  tools are subtracted on the server, by name, and the names are never sent.
- A connector whose every tool is hidden is not listed at all. It has nothing
  to offer in chat, and a switch that turns on nothing would mislead. A
  conversation that opted into it earlier keeps the opt-in, which reaches no
  tool.
- Turning a connector on can never expose a hidden tool by another path: the
  run gate above drops it from every chat turn, whatever the conversation
  opted into (`TestInteractiveHidden_ManagerRunTurnHidesTheTool` runs the turn
  with the connector opted in).

Both routes are chat-only; the task form reads the scheduler's own catalog,
which is unchanged. The Connections directory (`/mcp-catalog`) describes a
connector for every run, scheduled ones included, so it still counts a
chat-hidden tool. The web renders only what these routes return (search hits
come from the row's `tools`), so the filtering is all server-side.

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

**Name the tool, not the connector.** A member is a suffix of
`mcp_<server>_<tool>`. A named-account seat registers as
`mcp_<server>_<account>_<tool>`, so a member that pins the connector
(`openx_ox_create_deal` or `mcp_openx_ox_create_deal`) hides only the base
seat, and the raw tool stays callable from chat on every named seat. The bare
tool name (`ox_create_deal`) hides it on all of them. `fleet validate-config`
warns on a member that pins one of the bundle's connectors this way, and
names the tool to list instead. A tool whose own name starts with its
connector's name (`ix_merge_deal_domains` on `ix`) is not mistaken for one.

## Code

- `internal/agentcore/interactive_hidden.go`: the list, its validation and
  the refusal text.
- `internal/agentcore/mcp_tools.go`: the registration drop and the call gate.
- `internal/agentcore/tool_disclosure.go`: the `tool_describe` and
  `tool_call` explanations.
- `internal/agent/prompt.go`: `activeMCPToolNames`, the chat prompt's view of
  the roster.
- `internal/httpapi/connector_recommend.go`: the connector recommendation.
- `internal/agentcore/out_of_roster.go`: the direct-call explanation.
- `internal/agent/manager.go`, `interactive.go`, `subagent.go`,
  `scheduled.go`: the turn's list and its inheritance by sub-agents.
- `internal/httpapi/approval_card.go`: the describer skip.
- `internal/httpapi/approvals.go`: the email prevalidation skip
  (`prevalidateEmail`).
- `internal/httpapi/mcp_servers_hidden.go`: the chat Tools picker's rows.
- `cmd/fleet/validate_config.go`: the preflight.

Tests: `internal/agentcore/interactive_hidden_test.go`,
`internal/agent/interactive_hidden_test.go`,
`cmd/fleet/validate_config_hidden_test.go`,
`internal/clientconfig/interactive_hidden_tools_test.go`,
`TestStage_DescriberHiddenFromChatIsNotCalled`, and
`TestPrevalidateEmailSkipsInteractiveHiddenValidator`.
