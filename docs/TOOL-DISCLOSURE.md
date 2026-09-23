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
    too, but its answer reaches the model only as the broker's masked
    "credential-owner call failed", which reads as a broken credential rather
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
the bridges, names the connectors, and tells the model that a direct `mcp_*`
call will fail and `tool_call` is the way in. The section used to be written
by the driver before the roster existed, so above the threshold it listed
every name as callable — the model called one, got "tool not found", and gave
up on a working connector (#1006, four hosted connectors = 159 tools).

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
action, typed or legacy, naming an MCP tool the run did not register.

The live-registry section, the tool list the model is sent, and the disclosure
threshold count all see the narrowed roster. A narrowed roster usually lands
well under the threshold. A Pages data refresh drops from the 44 Pages tools
(about 135 KB of descriptions and schemas, ≈34K tokens resent on every step),
plus the `fast_io` and `fastio_helpers` tools, to the eight or so it names.

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
