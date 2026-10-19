# Calls to tools not in the tool list (#1744)

## Why

A model sometimes calls a tool name that is not in its tool list. Fantasy, the
agent framework, answers every such call with `tool not found: X. Available
tools: …` as a failed tool result, and the run goes on. That is right for a
made-up name, but it misleads in the two most common cases, where the name is a
real tool that was deferred ([`TOOL-DISCLOSURE.md`](TOOL-DISCLOSURE.md)) or
that a gate kept out of the run. In #1006 a model concluded from the refusal
that a working connector was down; a narrowed run (#1603) looped on it. And
nothing counted these calls, so there was no way to see which models emit them
or how often.

## What shipped

All of it is in `internal/agentcore/out_of_roster.go`:

- **A deferred tool called by name.** Models still call `mcp_*` names above the
  disclosure threshold. Now a direct call to a name in this run's deferred
  registry is rewritten into `tool_call {name, arguments}`, with the same call
  ID and the model's own argument bytes. It then runs through the bridge
  exactly as a `tool_call` the model wrote: persona and allowlist, credential
  allowlist, approval staging, audit and output governance. As for a
  hand-written `tool_call`, the assistant message, the transcript and the chat
  tool-call ledger built from it record `tool_call`, while the approval, the
  policy audit and the hooks name the real tool; so the model sees its call as
  a `tool_call`, and may say it "used `tool_call`". A test pins that the policy
  calls, staged approvals, broker dispatch and transcript are identical for the
  two, arguments with spacing and HTML characters included. Arguments get
  `tool_call`'s own leniency (malformed JSON is repaired; `null` and a
  one-object array are taken); others are not rerouted, and the result says how
  to call `tool_call`. Neither is a tool whose name has a character outside the
  MCP spec's tool-name set (ASCII letters, digits, `_`, `-`, `.`), since the
  name is written into `tool_call`'s input as it is. If the persona denies
  `tool_call` itself, nothing is rerouted: the live registry does not promise
  it, and the call's result says the tool is reachable only through
  `tool_call`, which is not in the tool list.
- **A tool a gate removed.** The roster records which gate kept each catalog
  tool out: an optional connector the run did not turn on (`opt_in`), a
  connector's tool allowlist (`allowlist`), a narrowed roster (`narrowed`,
  #1603), the persona's tool policy (`persona`, MCP and native tools), or an
  invalid input schema (`schema`). A call to one of these names is never run,
  and its result names the gate: "mcp_pages_deploy_page_upload is not available
  in this run and was not called: this run's tools are narrowed to the ones it
  requires, and mcp_pages_deploy_page_upload is not one of them." In a narrowed
  run it also names the registered stand-in, when there is one, by the rule the
  audit nudges use.
- **A tool the broker already dropped.** Out of process, the MCP broker
  applies a server's allowlist before the catalog reaches the run (ADR-0042),
  so a tool the allowlist or a narrowed roster dropped is never seen or
  recorded. An `mcp_*` name with no record is matched against the server names
  the run knows (the catalog's and the optional connectors', and in a narrowed
  run the allowlist's, which name the servers with no required tool) and run
  through the same opt-in and allowlist rules the build applies. A name alone
  cannot tell a dropped tool from a made-up one on the same connector, so the
  result keeps fantasy's text, with its list of tools to correct a typo from,
  and adds a sentence true of both: "The srv connector's tool allowlist does not
  include that name.", or the narrowing (with its stand-in) or the optional
  connector. The metric counts such a call as `unknown`, and the event names
  the gate it inferred.
- **Any other name** keeps fantasy's result and its list of available tools.

Rerouting cannot widen access. Only a name in the run's deferred registry is
rewritten, and only into `tool_call`, which checks the name against that
registry again and dispatches through the governed tool. A gated name never is:
the gates run before the registry is built, so it is never in it.

**How the explanation reaches the model.** Fantasy's repair hook can rewrite a
call, but cannot change the error of a call it leaves invalid. So the
explanation is kept by call ID, tool name, and the exact text fantasy refused
the call with, and replaces only a result that still has that text. A
provider may reuse a call ID, within a run and across turns, and the history
the model is sent includes earlier turns. Keyed by the ID and tool alone, the
failure of an earlier or later call that did run would read "was not called".
The run's transcript, and with it the next turn's history, records the
explanation in place of fantasy's text. Every later model call of the run is
sent it too: a prepare step rewrites a copy of each step's messages, and the
transcript a round carries on, to the next round, the finalize hook, the
terminal structured output and compaction, is rewritten the same way. It is
the same text every time, and the model first sees the result on the step
after the call, already explained, so the prompt-cache prefix does not change.

**Counted per model.** `fleet_tool_call_out_of_roster_total{model, reason}` on
`/metrics` counts every such call the model emits; `reason` is `deferred`, a
gate the roster recorded, or `unknown`. A `deferred` call is rerouted, or
explained when it cannot be; the event's `rerouted_to` tells the two apart.
Fantasy repairs a step's calls before it dispatches them, and dispatches only
when the step finished with tool calls: a call in a step that finished for
another reason (a provider's `stop`, say) is counted, and reported as
rerouted, though nothing ran. A step cut short by length or a content filter
is not repaired, so its calls are not counted. The tool name is not a label,
since catalog names can be user-controlled. It is in the
`fleet.tool_call_out_of_roster` Observer event (`id`, `tool`, `reason`,
`model`, `rerouted_to` when rerouted, and `inferred_gate` for a dropped tool's
gate), which chat streams as an event the web client ignores, and in a log
line.

Two details of the hook. Fantasy reads it from the stream call, not the agent,
so it is set on every stream, including the leaked-call finalize retry, which
streams with the run's tools and also sends the explanations. And installing a
hook turns off fantasy's default repair, `jsonrepair` of a registered tool's
malformed arguments; the run's hook keeps it.

## Deviations from the issue

- **Not every deferred name is rewritten.** The issue asked to rewrite any call
  whose name is in the deferred registry. Three are not, and their result says
  why: arguments `tool_call` would refuse anyway, a persona that denies
  `tool_call` itself, and a name with a character outside the MCP spec's
  tool-name set. A connector name an admin typed with a space in it (remote MCP
  server names are free text), or a tool named `files/read`, costs the model
  one more step this way: its result says to call `tool_call`, which works.
- **A tool the broker dropped is not named outright.** The issue asked for the
  gate to be named; for these the gate is inferred from the name, and a name
  alone fits a made-up tool on that connector just as well. So the result adds
  only a sentence true of both, and the metric counts the call as `unknown`
  rather than under the gate.

## Not done, deliberately

- The stand-in sentences in the narrowed run's audit nudges stay. They say more
  than the explanation (which records to call the stand-in with), and the issue
  expects the explanation to replace them only over time.
- No mapping of other harnesses' tool names (`str_replace_editor` →
  `edit_file`) and no per-model rosters; both are out of the issue's scope, and
  the metric is the data either would need.
- A tool missing for any other reason counts as `unknown`: a native tool the
  driver did not register, a tool an allowlist admits but the server does not
  have, or a tool on a server the run knows nothing about, such as one loaded
  mid-run with no allowlist entry of its own.
- Two calls in one step with the same ID and tool name: the last explanation
  wins.
