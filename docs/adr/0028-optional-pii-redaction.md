# ADR-0028: Optional, provider-neutral PII redaction at the tool-output choke point

- **Status:** Accepted
- **Date:** 2026-07-02
- **Deciders:** fleet maintainers

## Context

Issue #450 asks for an OPTIONAL way to strip PII from content before it reaches a
model, in both interactive chat and scheduled autonomous tasks, with strictness
modes and audit markers — provider-neutral (the Rampart ONNX model is one
possible implementation, not the interface). fleet already has an unconditional
SECRET scrubber (`internal/redact`) applied at the point "where external data
first enters" the model context: tool OUTPUT, the session log, and the SSE
stream. PII is a different class of data (opt-in, graded, auditable) but shares
the same threat shape (sensitive text flowing to an external model), so it should
reuse the same architecture rather than invent a new one.

Two hard constraints shaped WHERE the pass runs:

- **Prompt-cache prefix stability (#507).** The cacheable prefix (tool
  definitions + system prompt) must stay byte-stable across turns. A redaction
  pass over the whole outbound message list every round would mutate that prefix
  (and the incrementally-cached history), causing cache misses and cost/latency
  regressions.
- **Structured content.** A turn's messages carry structured parts — tool-call
  JSON arguments, tool results, reasoning — not just text. A regex pass over the
  serialized outbound messages could corrupt tool-call/tool-result JSON.

## Decision

Add `internal/piiredact`: a provider-neutral `Redactor` interface plus a
deterministic, dependency-free `PatternRedactor` (email, US SSN, credit card with
Luhn validation, IPv4 with octet validation, conservative NANP phone) that
returns `(redacted text, findings, blocked)`. Modes: **observe** (detect +
audit, pass through), **redact** (mask spans with `[PII:<kind>]`), **block**
(withhold the content wholesale). Findings carry the kind + count only — never
the raw value.

Wire it at the SAME choke point as the secret scrubber — **tool output** — in
`agentcore` (`policyGuardedTool.Run` for native tools, `mcpTool.Run` for MCP),
layered immediately after `toolRedactor().Redact(...)`. This placement:

- is **cache-safe**: it never touches the system-prompt prefix or tool
  definitions;
- is **structure-safe**: it operates on plain result text, never tool-call JSON
  args;
- covers the **highest-volume PII vector** for an agent platform — data pulled
  from connected systems (CRM records, emails, tickets) via tools;
- reuses the **one governed core**: it is an I/O pass on the same seam the secret
  scrubber already occupies, not a second governance path.

It is **default OFF**: `FLEET_PII_REDACTION_ENABLED` (default false) +
`FLEET_PII_REDACTION_MODE` install a process-wide redactor at boot
(`agentcore.SetPIIRedactor`); a nil redactor makes the pass a byte-for-byte
no-op. In **block** mode a detected tool result is withheld and flagged as an
error so the raw value never reaches the model.

## Consequences

- Enabling the feature scrubs PII from tool output before it enters the model
  context, the SSE stream, and the persisted session log (all fed from the same
  redacted content), with an audit trail of finding types/counts.
- No behavior change when disabled; no prompt-cache regression; no risk to
  structured tool-call content.
- The `Redactor` interface lets an external ONNX/HTTP classifier (Rampart)
  replace the built-in `PatternRedactor` with no call-site change.
- **Deviation / honest scope (documented, not silently missing):** this first
  cut covers the tool-output boundary only. The user's own chat/task PROMPT, the
  assistant's own generated text, tool ARGUMENTS, notifications, eval goldens,
  and browser OCR are **follow-on** boundaries. The built-in detector is a
  deterministic aid, not a certified DLP engine — it will miss unusual PII shapes
  and may false-positive; regex phone detection is deliberately conservative. The
  external ONNX/Rampart classifier is not shipped (interface-ready). Because the
  detector is imperfect, an invalid configured mode defaults to `redact` (the
  control stays on) rather than failing boot.

## Alternatives considered

- **Redact the whole outbound message list at `engine.stream()`.** The recon's
  "single choke point," but rejected: it breaks the prompt-cache prefix-stability
  contract (#507) and risks corrupting structured tool-call/tool-result JSON, and
  re-runs the regex over the full history every round.
- **Redact at chat-input / task-prompt ingestion.** Valuable but requires
  careful handling of history persistence vs replay to avoid double-redaction and
  cache churn; deferred to a follow-on. Tool output is the higher-volume,
  lower-risk first boundary and matches the existing secret-scrubber seam.
- **Rampart-only (ONNX) implementation.** Rejected per triage: the feature must
  be provider-neutral; Rampart is one pluggable impl behind the `Redactor`
  interface.
