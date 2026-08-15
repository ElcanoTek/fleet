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
| `FLEET_PII_REDACTION_ENGINE` | `pattern` | `pattern` \| `rampart` (needs the URL below) |
| `FLEET_PII_RAMPART_URL` | — | Rampart detection service endpoint |

Modes:
- **observe** — detect and audit-log findings (kind + count, never the raw
  value), but pass the text through unchanged. A monitoring posture.
- **redact** — replace each detected span with a `[PII:<kind>]` marker.
- **block** — withhold the tool result wholesale (replace with a
  `[BLOCKED: …]` notice) and flag it as an error, so the raw value never reaches
  the model.

An enabled-but-unset or invalid mode defaults to `redact` — a misconfiguration
keeps the control ON rather than silently disabling it.

## Detection engines

Two engines implement the same `piiredact.Redactor` contract; pick one from
the admin panel (`pii_redaction_engine`) or `FLEET_PII_REDACTION_ENGINE`:

### `pattern` (default) — built-in deterministic redactor

Email, US SSN (hyphenated), credit-card numbers (Luhn-validated to reject
arbitrary digit runs), IPv4 (octet-range validated), and conservative NANP phone
numbers (a separator is required, so a bare digit id isn't swept up). Zero
dependencies, no model server. Matches are replaced with flat `[PII:<kind>]`
markers.

### `rampart` — ML detection via an external service

[Rampart](https://huggingface.co/nationaldesignstudio/rampart) is a 14.7 MB
MiniLM token-classification ONNX model covering **17 PII entity types** —
given names/surnames, phone, email, URL, IP, SSN, credit card, government ID,
passport, driver's license, tax ID, bank account, routing number, and street
address components — far beyond the pattern engine's five shapes. It runs
**out of process** behind a small HTTP service you deploy next to fleet;
[`scripts/rampart-service`](../scripts/rampart-service/README.md) is the
reference implementation over the official npm runtime (~25 ms per call on
CPU). Three ways to host it, easiest first:

1. **One click in the admin panel.** Settings → Admin → Feature settings →
   **Install Rampart service**. fleet builds the service container (the
   ONNX model is baked into the image, so the service needs no runtime
   network), runs it on loopback, health-checks it, fills in the service URL,
   and re-starts it after a box reboot. Nothing to install first — fleet
   already runs rootless podman for its sandbox, and the service build context
   is embedded in the fleet binary (so it ships and updates with fleet; no
   bootstrap/update changes needed). Then switch the engine to Rampart and
   click **Test detection**.
2. **`scripts/rampart-service/install.sh`** — the same container under a
   systemd unit you own, for operators who prefer that.
3. **Manual** (`npm start`, or the Containerfile) — see the service README.

Either way, configure the endpoint via `pii_rampart_url` /
`FLEET_PII_RAMPART_URL` (e.g. `http://127.0.0.1:8787/v1/redact`) — option 1
fills it in for you.

The service contract is deliberately text-in/text-out (no offset math across
the process boundary):

```
POST <url>  {"text": "..."}
200         {"text": "<redacted>", "findings": [{"kind": "given_name", "count": 1}, ...]}
```

Rampart redacts with **stable numbered placeholders** (`[GIVEN_NAME_1]`,
`[SSN_1]`) so the model can still refer to entities distinctly. Two posture
guarantees:

- **Strict superset of the pattern floor.** The deterministic engine sweeps
  Rampart's output as a second pass, so a structured shape the model misses
  (a formatted phone number, in live testing) is still caught.
- **Never fail-open.** If the service is unreachable or answers garbage, the
  call falls back to the pattern engine (rate-limited log; the "Test
  detection" button surfaces the outage). A restart with
  `FLEET_PII_REDACTION_ENGINE=rampart` but no URL degrades to `pattern`,
  never to off.

The service sees raw tool output — bind it to loopback or a private network,
same trust domain as fleet itself.

**Either engine is a redaction aid, not a certified DLP engine.** Detection
can miss unusual shapes and can false-positive. Use the admin panel's **Test
detection** button (`POST /admin/pii-redaction/test`) to run the live redactor
over a synthetic sample and see the engine, detected kinds, marker style, and
latency.

## Honest scope / deferred

This first cut covers the **tool-output** boundary only. Follow-ons (documented,
not silently missing):

- The user's own chat / scheduled-task **prompt** and the assistant's own
  generated text (ingestion-side redaction with careful history-persistence
  handling).
- Tool **arguments**, notifications (#292), eval goldens (#502).
- Per-conversation / per-task mode overrides (the admin setting is
  workspace-global).
