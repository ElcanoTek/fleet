# Optional PII redaction (#450)

fleet can OPTIONALLY strip PII from tool output before it enters the model
context, complementing the unconditional secret scrubber (`internal/redact`).
It is **default OFF**, provider-neutral, and deterministic (no model server
required). See [`adr/0028-optional-pii-redaction.md`](adr/0028-optional-pii-redaction.md)
for the design rationale.

## What it does

When enabled, every tool result — the highest-volume vector for PII entering an
agent's context (connector records, emails, tickets) — passes through a PII
redactor at the same choke point the secret scrubber already occupies
(`agentcore` tool wrappers). The redacted text is what re-enters the model
context, the SSE stream, and the persisted session log.

It operates on plain result **text** only — never the cacheable system-prompt
prefix or structured tool-call JSON arguments — so the prompt-cache
prefix-stability contract (#507) is preserved and tool-call structure is never
corrupted.

## Configuration

| Env var | Default | Meaning |
| --- | --- | --- |
| `FLEET_PII_REDACTION_ENABLED` | `false` | Master switch. Off = byte-for-byte unchanged. |
| `FLEET_PII_REDACTION_MODE` | `redact` (when enabled) | `observe` \| `redact` \| `block` |

Modes:
- **observe** — detect and audit-log findings (kind + count, never the raw
  value), but pass the text through unchanged. A monitoring posture.
- **redact** — replace each detected span with a `[PII:<kind>]` marker.
- **block** — withhold the tool result wholesale (replace with a
  `[BLOCKED: …]` notice) and flag it as an error, so the raw value never reaches
  the model.

An enabled-but-unset or invalid mode defaults to `redact` — a misconfiguration
keeps the control ON rather than silently disabling it.

## What it detects (built-in deterministic redactor)

Email, US SSN (hyphenated), credit-card numbers (Luhn-validated to reject
arbitrary digit runs), IPv4 (octet-range validated), and conservative NANP phone
numbers (a separator is required, so a bare digit id isn't swept up).

**This is a redaction aid, not a certified DLP engine.** It will miss unusual PII
shapes and can false-positive; phone detection is deliberately conservative to
limit noise. For stronger detection, the `piiredact.Redactor` interface accepts a
pluggable implementation — e.g. an external ONNX classifier such as
[Rampart](https://huggingface.co/nationaldesignstudio/rampart) behind an HTTP
service — as a follow-on with no change to the wiring.

## Honest scope / deferred

This first cut covers the **tool-output** boundary only. Follow-ons (documented,
not silently missing):

- The user's own chat / scheduled-task **prompt** and the assistant's own
  generated text (ingestion-side redaction with careful history-persistence
  handling).
- Tool **arguments**, notifications (#292), eval goldens (#502), browser OCR.
- The external ONNX/Rampart classifier implementation (interface-ready).
- Per-conversation / per-task mode overrides.
