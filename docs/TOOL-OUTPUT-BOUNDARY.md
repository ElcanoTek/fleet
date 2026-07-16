# Bounded model-visible tool output

Fleet applies one final output policy to every `fantasy.AgentTool` registration
route before a result can re-enter model context. The policy is shared by
interactive and scheduled work because it is wired inside the single
`agentcore.Run` core; it does not create another agent loop or execution path.

## Per-result boundary

`FLEET_MAX_TOOL_OUTPUT_BYTES` and the live `max_tool_output_bytes` setting select
an operational limit. The built-in default is 64 KiB. Values below 1 KiB are
raised to 1 KiB so a valid envelope always fits, and values above 128 KiB are
lowered to the non-disableable 128 KiB hard maximum. Zero and negative env
values select the 64 KiB default; zero is no longer a kill switch. A negative
value passed to the internal live-setting setter means “clear the override,”
after which the normalized env/default value applies.

The final wrapper covers native tools, scheduled loader tools, direct MCP,
hidden MCP invoked through `tool_call`, and Fantasy media responses. Direct and
deferred MCP wrap the hidden logical tool itself, so tool
disclosure changes visibility but not output policy.

Text output is rendered as a bounded head/tail preview. JSON output is rendered
as compact, valid JSON rather than an arbitrary slice. Envelopes report:

- a Fleet envelope-version marker for structured JSON;
- tool and format;
- original and shown byte counts;
- `truncated: true`;
- whether encoded binary was suppressed;
- an optional workspace-relative artifact path; and
- a concrete recovery action.

Large base64/data-URI fields and Fantasy image/media bytes receive no inline
binary preview. `run_python` follows the same contract: a large `vars` value can
never become partial JSON or partial base64. Models should write large values to
a workspace file and pass the path to the next tool.

## Governed artifact recovery

For oversized text/JSON, agentcore offers the complete content to a narrow
`ModelOutputArtifactStager` only after the route has applied secret redaction,
optional PII policy, and the host guardrail. There is no default host writer.
Interactive runs and isolated scheduled worktrees install a stager only after
acquiring the run's live sandbox and resolving its exact private root. Shared
non-worktree scheduled runs bind their sandbox/file-tool root but deliberately
install no artifact stager because an individual task does not own that root.
Missing scope or a staging failure leaves the hard cap intact and returns a
narrower-query recovery action instead of falling back to `/tmp`.

Retained bytes are written through the confined `Sandbox.RunFileOp` seam under:

```text
<effective workspace>/.fleet/tool-output/slot-00/artifact-<full sha256>.txt
...
<effective workspace>/.fleet/tool-output/slot-15/artifact-<full sha256>.txt
```

The returned path is workspace-relative, so the sandbox-bound `view_file` tool
can read it in chunks through the same model-visible capability. Interactive
runs bind to the private conversation workspace; isolated scheduled runs bind
to their worktree. All scheduled modes still bind bash, Python, and file tools
to the reported effective root, but only an isolated worktree has a retained
recovery path. The file-op request carries that root as a confinement
capability; the executor rejects traversal and symlink escapes rather than
following a host-validated pathname.

Retention is deliberately finite. Governed artifacts larger than 8 MiB are not
created, and each private conversation/worktree workspace has at most 16
retained recovery-slot directories at a time. Each advertised name includes the
full SHA-256 digest of its governed bytes, and the containing slot directory is
also its durable allocation tombstone. Fleet never assigns an advertised path
to different content, even if a workspace tool deletes the cursor or an
artifact before a sandbox or process restart. Deleting the whole slot directory
explicitly frees its storage and capacity; that necessarily deletes its child
artifact, and different later bytes still produce a different historical path.
Thus allocator resets cannot accumulate more than 16 retained files. Without
explicit deletion, the seventeenth result remains capped and receives a
narrower-query recovery action without an artifact. Fleet never overwrites any
of the 16 paths while they remain present, so parallel tool results keep their
promised bytes. The sandbox-written cursor and slot directories preserve that
capacity decision across loop passes, sandbox recreation, and process restarts,
and workspace cleanup removes the files.
Shared non-worktree scheduled roots deliberately disable artifact retention
because concurrent tasks do not own that root; their hard result cap still
applies. For a Git workspace, a fixed
`.fleet/tool-output/.gitignore` is written through the same confined FileOp seam
before the first artifact. Its `*` pattern keeps the cursor and recovery slots
out of `git add -A` in both main and linked worktrees without mutating repository
Git metadata. If setup fails, Fleet keeps the output cap but disables artifact
retention for that call.

## Inner-step aggregate budget

The per-result cap is necessary but insufficient: many legal results can
accumulate during one Fantasy tool loop. Before every inner provider call Fleet
now computes:

```text
message target = model context window
               - system-prompt estimate
               - registered-tool-schema estimate
               - configured completion allowance
               - max(4,096 tokens, 5% of the window)
```

Message accounting includes ordinary text, tool result text/errors/media,
assistant tool-call inputs, file parts, and per-message/part framing. The active
Fantasy model supplies the window, so a fallback swap is recalculated against
the fallback rather than the original model. OpenRouter uses observed or
catalog metadata. Native providers use the manifest's
`context_window_tokens`; when omitted, Fleet fails safe at 4,096 for Ollama and
32,000 for other native/OpenAI-compatible providers rather than assuming the
OpenRouter 200K fallback.

If messages exceed the target, Fleet reduces oldest tool results to 2 KiB
envelopes, replaces old tool-call inputs with valid JSON recovery sentinels, and
then evicts more old result preview down to 640 bytes until the request fits.
Recent results remain intact once older payloads provide enough room. The
change is request-local: persisted history is not rewritten, and the same guard
runs whenever that history is replayed. If non-tool content alone makes the
reserved target impossible, Fleet refuses the request with
`ErrInnerContextBudgetExceeded` instead of knowingly sending an oversized
provider request.

## Observability

The Prometheus endpoint exposes:

- `fleet_tool_output_truncations_total{tool,format}` (`tool` is the bounded
  route class `native`, `mcp`, `disclosure`, or `unknown`; exact tool names stay
  in logs rather than unbounded metric labels);
- `fleet_tool_output_artifacts_total{result}` (`success`, `unavailable`,
  `capacity`, or `failure`);
- `fleet_tool_context_reductions_total{kind}`;
- `fleet_tool_context_estimated_tokens{model,phase}`; and
- `fleet_tool_context_pressure_ratio{model,phase}`.

Aggregate reductions also emit `fleet.tool_context_reduced` on the existing run
observer with before/after estimates, reserves, and reduction counts. The UI's
4,000-byte stream preview remains independent; it does not define what the model
or persisted replay receives.

## Host-I/O consolidation

The older bash, `run_python`, and `web_fetch` 32 KiB temp-spill layer and the
agent driver's fixed 6 MiB overflow-file PrepareStep were removed. They could
write before final governance, returned paths that sandboxed `view_file` could
not reach, and duplicated context policy. Raw bounded-capture results now reach
the policy wrapper in memory; the final boundary alone renders model-visible
truncation and, when safe, retains the governed form through `RunFileOp`.

This does not weaken execution capture limits. Bash still stops retaining bytes
at its 64 MiB stdout/stderr safety cap and honestly reports discarded counts;
bytes discarded by the executor never become an artifact and are never claimed
as recoverable. Python's bridge likewise reports its own capture truncation.
