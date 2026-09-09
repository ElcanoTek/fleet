# Optional PII redaction (#450)

fleet can OPTIONALLY strip PII from tool output before it enters the model
context, complementing the unconditional secret scrubber (`internal/redact`).
It is **default OFF**, provider-neutral, and deterministic (no model server
required). See [`adr/0028-optional-pii-redaction.md`](adr/0028-optional-pii-redaction.md)
for the design rationale, and
[`adr/0063-remove-the-rampart-pii-engine.md`](adr/0063-remove-the-rampart-pii-engine.md)
for why the external ML engine that once sat behind the same interface is gone.

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

**From the web UI (recommended):** Settings → Admin → Feature settings →
**PII redaction** — pick `off` / `observe` / `redact` / `block`. The change
applies to the very next tool call, no restart, and an admin override wins
over the env vars below until it is reset. See
[ADMIN-SETTINGS.md](ADMIN-SETTINGS.md).

**From the env file (the deployment default):**

| Env var | Default | Meaning |
| --- | --- | --- |
| `FLEET_PII_REDACTION_ENABLED` | `false` | Master switch. Off = byte-for-byte unchanged. |
| `FLEET_PII_REDACTION_MODE` | `redact` (when enabled) | `observe` \| `redact` \| `block` |

`FLEET_PII_REDACTION_ENGINE` and `FLEET_PII_RAMPART_URL` are retired
(ADR-0063). A value in either still loads, does nothing, and is named in a
boot warning so a stale env file is not silently ignored.

Modes:
- **observe** — detect and audit-log findings (kind + count, never the raw
  value), but pass the text through unchanged. A monitoring posture.
- **redact** — replace each detected span with a `[PII:<kind>]` marker.
- **block** — withhold the tool result wholesale (replace with a
  `[BLOCKED: …]` notice) and flag it as an error, so the raw value never reaches
  the model.

An enabled-but-unset or invalid mode defaults to `redact` — a misconfiguration
keeps the control ON rather than silently disabling it.

## Detection engine

One engine implements the `piiredact.Redactor` contract: the built-in
deterministic redactor. Email, US SSN (hyphenated), credit-card numbers
(Luhn-validated to reject arbitrary digit runs), IPv4 (octet-range validated),
and conservative NANP phone numbers (a separator is required, so a bare digit
id isn't swept up). Zero dependencies, no model server. Matches are replaced
with flat `[PII:<kind>]` markers.

The interface stays provider-neutral — an external classifier could implement
the same `Redact(text) Result` contract — but none is shipped. The Rampart ML
engine (a MiniLM token-classification model behind an operator-deployed HTTP
service, with a one-click installer and an in-repo reference service) was
removed in [ADR-0063](adr/0063-remove-the-rampart-pii-engine.md): nobody was
running it, and its npm dependency tree carried a vulnerability no upstream
release could clear. An upgraded deployment that had `pii_redaction_engine =
rampart` persisted simply runs the pattern engine at its saved mode; the two
retired setting rows are never read again and can be ignored.

**The engine is a redaction aid, not a certified DLP engine.** Detection can
miss unusual shapes and can false-positive.

## Honest scope / deferred

This first cut covers the **tool-output** boundary only. Follow-ons (documented,
not silently missing):

- The user's own chat / scheduled-task **prompt** and the assistant's own
  generated text (ingestion-side redaction with careful history-persistence
  handling).
- Tool **arguments**, notifications (#292), eval goldens (#502).
- Per-conversation / per-task mode overrides (the admin setting is
  workspace-global).
