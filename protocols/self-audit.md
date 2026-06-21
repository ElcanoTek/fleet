# Self-Audit Protocol

Mandatory QA step before executing critical actions or finishing the task.

Ensure outputs are accurate, data-driven, formatted, and complete — without forcing irrelevant checks on tasks that do not need them.

## How to use

1. Identify which task type(s) apply to the current run. Multiple may apply.
2. Run only the relevant section(s). Skip checks that do not apply.
3. Always run **universal** and **completion gate**.
4. Call `confirm_audit` before any critical action (mcp_sendgrid_send_email, generate presentation, mutating MCP write). The task that performs the critical action MAY remain in_progress at audit time — `confirm_audit` gates the action, and the task is marked done immediately after the action returns.

## Task type selector

| Type | When | Run these checklists |
|------|------|---------------------|
| Analysis / report | Task involves loading data files, running analytics, or producing a report | data_integrity, output_quality, universal |
| Email / messaging | Task sends an email or other outbound message | output_quality, universal |
| Mutation / deal creation | Task creates/updates an SSP/DSP object via MCP | tool_usage, universal |
| Simple / conversational | Short instruction with no data, e.g. "say hello", "draft a note" | universal |

## Checklists

### Data integrity
Only run when the task loaded data or made quantitative claims.

- **real_data_used** — Did I load actual data files or use web_search? (FAIL if guessed numbers)
- **full_processing** — Did I process the entire dataset, not just a sample? (FAIL if nrows= used)
- **multi_source_coverage** — If multiple data sources were requested, did I query every one?
- **multi_source_follow_through** — If I quit a source early, did I return and finish it?
- **verification** — Did I cross-reference unusual findings with web_search?

### Output quality
Run for any task that produces a user-facing artifact.

- **email_formatting** — If sending email, is it styled HTML using a registered theme? (FAIL if plain text)
- **recipient_completeness** — If multiple recipients were requested, did I include every one?
- **visuals** — Did I include charts or tables where they would help the reader?
- **answer_completeness** — Did I answer every part of the user's prompt?

### Tool usage
Run for tasks that call out specific tools or protocols.

- **tools_used** — Did I use the specific tools the user requested?
- **protocol_adherence** — If a protocol was named, did I follow its required steps?
- **download_attempts** — If emails had links, did I try downloading before assuming auth was required?

### Universal
Always run.

- **task_tracker_progress** — Are all non-critical-action tasks marked "done"? (Tasks that perform a gated critical action MAY remain in_progress until the action completes.)
- **request_reread** — Have I re-read the original request and confirmed every deliverable is produced or queued?

## Execution

1. Pick the relevant checklist section(s) from the task type selector.
2. Walk the items in those sections plus **universal**.
3. If issues are found, fix them in place before calling `confirm_audit`.
4. Call `confirm_audit` with structured evidence including reasoning, artifacts_checked, workflow_sections_checked, the critical-actions field (see below), send_contract_checked, attachments_checked, and remaining_risks.
   - **Preferred (typed): `critical_actions`.** A list of `{tool, identifier}` objects, one per MCP tool this audit unlocks. `tool` is the literal tool name copied verbatim from your tool list (e.g. `mcp_openx_mcp_ox_create_prepared_deal`); `identifier` is an optional human-readable tag (deal name, recipient, etc.) used only for logging. The orchestration matches on `tool` directly — no parsing, no guesswork.
     ```yaml
     critical_actions:
       - tool: mcp_openx_mcp_ox_create_prepared_deal
         identifier: AdGreetings_PG_25
       - tool: mcp_sendgrid_send_email
         identifier: trader@elcanotek.com
     ```
   - **Legacy (deprecated): `critical_actions_being_unblocked`.** A list of free-text strings still supported for backward compatibility with older protocols. Each entry MUST contain the literal tool name as a substring (e.g. `mcp_openx_mcp_ox_create_prepared_deal: AdGreetings_PG_25`) so the substring matcher can extract a known suffix. Do not paraphrase — natural language like "Create the OpenX deal and send the report" will not register as a commitment, and the audit token will consume on the first critical execution, blocking subsequent critical calls.
   - **Use `critical_actions` (the typed form) for any new code or updated protocol.** Use `critical_actions_being_unblocked` only when working inside an older protocol that hasn't migrated yet. Never supply both — the typed form takes precedence and the legacy entries are ignored.
5. If the task cannot be completed safely, call `confirm_audit(success=false, ...)` with the same structured evidence plus a user_visible_summary explaining the blocker and what remains unresolved.

## Completion gate

Only after a successful `confirm_audit` can you:
- Send emails
- Generate presentations
- Execute mutating MCP write tools
- Finish the task

Calling `confirm_audit` a second time is unnecessary and discouraged unless the output artifact materially changed after the first audit.
