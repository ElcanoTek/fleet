# Prompt-injection guardrails (#702)

Fleet can optionally screen untrusted text before it enters the model context.
The check runs host-side, outside the sandbox and agent loop, so a persona,
skill, tool, or model cannot disable it.

It covers user and scheduled-task messages before the first provider request,
plus native, MCP, HTTP, file, and other tool output at the shared tool
result boundary. The operator-trusted system prompt is not screened or mutated,
which preserves the prompt-cache prefix contract.

## Configure

Deploy an HTTP detector on loopback or a private network. Fleet sends:

```json
{"profile":"prompt-injection","source":"user_message","text":"..."}
```

The endpoint returns:

```json
{"flagged":true,"score":0.98,"reason":"instruction override"}
```

Configure it from Settings → Admin → Feature settings, or with:

```text
FLEET_GUARDRAIL_URL=http://127.0.0.1:8790/v1/check
FLEET_GUARDRAIL_MODE=observe
FLEET_GUARDRAIL_PROFILE=prompt-injection
```

`off` is the backward-compatible default. `observe` passes content and records
flagged verdicts or detector outages without logging the content. `block`
withholds flagged content; it also fails closed when the detector is unavailable.
The admin-only `POST /admin/guardrail/test` endpoint checks the live detector
with a fixed synthetic injection sample.

## Security scope

A classifier is probabilistic and can false-positive or miss an attack. It is
defense in depth, not an authorization boundary and not a replacement for the
mandatory sandbox, host-side credential broker, approval cards, egress policy,
or run ceilings. Blocked tool text is replaced before it can reach the model or
turn history. Seed-message blocks happen before any provider call.

The detector receives raw text. Keep it in Fleet's trust domain and protect its
transport accordingly. Fleet records source, profile, mode, score, and outcome,
never the screened text.

## Honest scope

- Policy is workspace-global; there are no task/persona exceptions in v1.
- Images are not decoded or OCR-screened. Their accompanying text is screened.
- The detector contract supports named profiles, but the admin surface starts
  with the deployment-wide `FLEET_GUARDRAIL_PROFILE`.

