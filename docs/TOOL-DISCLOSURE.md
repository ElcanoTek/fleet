# BM25 progressive tool disclosure

A large MCP catalog used to be a hard error: over 128 tools blew the provider's
per-request ceiling, and every tool's schema was billed on every turn. #506
removes the ceiling and cuts per-turn tokens by deferring most tools behind
three bridge tools backed by an in-process BM25 keyword index — no embeddings,
no vector DB, no network. (ADR-0026.)

## How it works

`buildFantasyTools` (the ONE builder both interactive and scheduled runs feed)
counts the roster it would register. When that exceeds the disclosure threshold
(`FLEET_TOOL_DISCLOSURE_THRESHOLD`, default 128 = the provider ceiling; also
tunable live from Settings → Admin → Feature settings, see
[ADMIN-SETTINGS.md](ADMIN-SETTINGS.md)), it:

- keeps **core tools** directly registered — native (bash/python/files/…),
  loader, and confirm_audit are NEVER deferred;
- hides the **MCP tools** (those that already passed the opt-in + allowlist
  gates) behind three bridges:
  - `tool_search {query}` — BM25 keyword search over `{name, description}` →
    top-K names + one-line descriptions;
  - `tool_describe {name}` — the tool's full description + its JSON parameter
    schema as the provider would see it for a direct tool: `type`,
    `properties` **and `required`**, plus a one-line "Required arguments"
    summary (the properties map alone was printed until the #1006 catalog
    audit, and a model could not learn that Stripe's every API tool needs
    `stripe_context` and `livemode`);
  - `tool_call {name, arguments}` — dispatches to the real tool's `Run`, after
    refusing — with the missing names spelled out — a call that omits an
    argument the tool's schema marks required. The vendor would refuse it
    too — and since F8 its 4xx answer reaches the model through the broker
    — but that costs a vendor round trip; refusing here is free, and before
    F8 the vendor's answer arrived only as the broker's masked
    "credential-owner call failed", which read as a broken credential rather
    than a fixable call. `required` means *present*: a required argument sent
    as JSON `null` is refused only when the property's own schema does not
    admit null. The schema's keywords apply together, as JSON Schema says:
    `type: ["string","null"]` or `nullable: true` admits it unless a sibling
    `enum`, `const` or `not` excludes it; `anyOf` needs one arm that admits
    it, `oneOf` exactly one, `allOf` every one; an unconstrained schema (`{}`
    or the boolean `true`) admits everything. Only a schema that *provably*
    excludes null refuses: a construct fleet does not evaluate locally
    (`$ref`, `if`/`then`/`else`, `dependentSchemas`) leaves the verdict
    unknown, and unknown lets the call through to the vendor. So the deferred
    path never refuses a call the direct path would have executed, and never
    passes a null the vendor's own schema plainly forbids.

A deferred `tool_call` routes through the **same `*mcpTool` wrapper** a direct
call would, so the MCP broker + per-task credential allowlist (#184), the policy
gate (BeforeToolCall/RecordToolResult), output redaction + ceiling, and audit
all apply identically — a deferred tool is first-class, just not always
advertised.

Below the threshold nothing changes: every tool registers directly, byte-for-
byte as before (so the #507 prompt-cache prefix stays intact for small
catalogs).

**The system prompt follows the roster, not the other way round.** The
"MCP Tools (live registry)" section the model reads is appended by
`agentcore.Run` *after* `buildFantasyTools` returns, from what it actually
registered (`internal/agentcore/live_registry.go`): below the threshold it
lists the `mcp_*` names verbatim; above it, it says how many tools sit behind
the bridges, names the connectors, and tells the model to use `tool_call` (a
direct `mcp_*` call is rerouted through it, see below). The section used to
be written by the driver before the roster existed, so above the threshold it
listed every name as callable — the model called one, got "tool not found",
and gave up on a working connector (#1006, four hosted connectors = 159 tools).

## Narrowing the roster instead (#1603)

Disclosure changes how much of the roster the model sees on each turn, not what
the roster contains. A run that knows exactly which MCP tools it needs can
shrink the roster itself. A scheduled task's `EXECUTION REQUIREMENTS` may carry
`"roster":"required_tools_only"` (see
[CONDITIONAL-TASK-COMPLETION.md](CONDITIONAL-TASK-COMPLETION.md#copyable-execution-prerequisites)).

The scheduled driver then intersects each server's Gate-2 allowlist with the
tools `required_tools` names, and makes Gate-2 **exhaustive and exact** for the
run (`RunConfig.MCPRosterNarrowing`): a selected server none of whose tools is
required registers nothing, and so does a server loaded mid-run that has no
entry of its own, such as a `mcp_load_servers(client=…)` seat. Native, loader
and `confirm_audit` tools are untouched, but `confirm_audit` refuses a critical
action, typed or legacy, naming an MCP tool the run did not register and that
no registered alias twin or approved substitute stands in for.

The live-registry section, the tool list the model is sent, and the disclosure
threshold count all see the narrowed roster. A narrowed roster usually lands
well under the threshold. A Pages data refresh drops from the 44 Pages tools
(about 135 KB of descriptions and schemas, ≈34K tokens resent on every step),
plus the `fast_io` and `fastio_helpers` tools, to the eight or so it names.

## Calls to tools not in the list (#1744)

A model sometimes calls a tool name that is not in its tool list. Fantasy, the
agent framework, answers every such call with `tool not found: X. Available
tools: …` as a failed tool result, and the run goes on. That is right for a
made-up name, but it misleads when the name is a real tool that was deferred,
or that a gate kept out of the run (`internal/agentcore/out_of_roster.go`):

- **A deferred tool called by name.** Models still call `mcp_*` names above
  the threshold, and in #1006 one concluded from the refusal that the connector
  was down. Now a direct call to a name in this run's deferred registry is
  rewritten into `tool_call {name, arguments}`, with the same call ID and the
  model's own argument bytes. It then runs through the bridge exactly as a
  `tool_call` the model wrote: persona and allowlist, credential allowlist,
  approval staging, audit and output governance, and the transcript records it
  as a `tool_call`. A test pins that the policy calls, staged approvals, broker
  dispatch and transcript are identical for the two, arguments with spacing and
  HTML characters included. Arguments get `tool_call`'s own leniency (malformed
  JSON is repaired; `null` and a one-object array are taken); others are not
  rerouted, and the result says how to call `tool_call`. If the persona denies
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
explanation is kept by call ID and tool name. A provider may reuse a call ID,
and keyed by the ID alone a later failure of a tool that did run would read
"was not called". The run's transcript, and with it the next turn's history,
records the explanation in place of fantasy's text. Every later model call of
the run is sent it too: a prepare step rewrites a copy of each step's
messages, and the transcript a round carries on, to the next round, the
finalize hook, the terminal structured output and compaction, is rewritten
the same way. It is the same text every time, and the model first sees the
result on the step after the call, already explained, so the prompt-cache
prefix does not change.

**Counted per model.** `fleet_tool_call_out_of_roster_total{model, reason}` on
`/metrics` counts every such call the model emits; `reason` is `deferred`, a
gate the roster recorded, or `unknown`. Fantasy repairs a step's calls before
it dispatches them, and dispatches only when the step finished with tool
calls: a call in a step that finished for another reason (a provider's `stop`,
say) is counted, and reported as rerouted, though nothing ran. A step cut
short by length or a content filter is not repaired, so its calls are not
counted. The tool name is not a label, since catalog names can be
user-controlled. It is in the `fleet.tool_call_out_of_roster` Observer event
(`id`, `tool`, `reason`, `model`, `rerouted_to` when rerouted, and
`inferred_gate` for a dropped tool's gate), which chat streams as an event the
web client ignores, and in a log line.

Two details of the hook. Fantasy reads it from the stream call, not the agent,
so it is set on every stream, including the leaked-call finalize retry, which
streams with the run's tools and also sends the explanations. And installing a
hook turns off fantasy's default repair, `jsonrepair` of a registered tool's
malformed arguments; the run's hook keeps it.

Not done, deliberately:

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

## BM25 index (`internal/tools/bm25_index.go`)

Textbook BM25 (k1=1.5, b=0.75) over tokenized tool metadata. The tokenizer
splits `snake_case` and `camelCase` so "send email" matches both `send_email`
and `sendEmail`. It's pure, deterministic, and dependency-free — the same index
is reusable for connector recommendation (#512).

## Honest scope

- Deferral triggers on COUNT, not on token budget — a large-but-under-threshold
  catalog still registers directly (raise/lower `FLEET_TOOL_DISCLOSURE_THRESHOLD`
  to tune; e.g. set it below your typical roster to always defer and shave
  tokens).
- BM25 is keyword ranking; it won't match a purely semantic query with zero
  lexical overlap. For tool names/descriptions that's rarely an issue, and the
  model can re-query with different keywords.
- Name collisions across servers resolve last-write-wins in the deferred
  registry, matching direct registration.
