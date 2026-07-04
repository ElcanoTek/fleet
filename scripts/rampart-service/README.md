# fleet Rampart PII detection service

The reference implementation of fleet's PII detection-service contract
([docs/PII-REDACTION.md](../../docs/PII-REDACTION.md)), wrapping the official
[Rampart](https://huggingface.co/nationaldesignstudio/rampart) runtime — a
14.7 MB MiniLM token-classification ONNX model detecting 17 PII entity types
(names, contact info, government IDs, financial numbers, street addresses).

## Host it (one command)

```sh
scripts/rampart-service/install.sh
```

Builds the container with rootless podman (the ONNX model is baked in at
build time), installs + starts a systemd unit (`fleet-rampart.service`,
user-level unless run as root), health-checks it, and prints the URL to paste
into the admin panel. `--uninstall` reverses it. Requires only what a fleet
box already has: podman + systemd — no Node on the host.

## Run directly (development)

```sh
cd scripts/rampart-service
npm install
npm start          # loads the model (~1s; first run downloads ~15 MB), listens on 127.0.0.1:8787
```

Then in fleet: Settings → Admin → Feature settings → set **Rampart service
URL** to `http://127.0.0.1:8787/v1/redact`, switch **PII detection engine** to
*Rampart*, and click **Test detection**.

Or as a container:

```sh
podman build -t fleet-rampart-service .
podman run -d --name rampart -p 127.0.0.1:8787:8787 -e RAMPART_ADDR=0.0.0.0:8787 fleet-rampart-service
```

## Contract

```
POST /v1/redact   {"text": "..."}
200               {"text": "<redacted>", "findings": [{"kind": "given_name", "count": 1}, ...]}
GET  /healthz     {"ok": true}
```

The redacted text uses Rampart's stable numbered placeholders
(`[GIVEN_NAME_1]`, `[SSN_1]`, …). `findings` counts distinct entities per
label. Any service that speaks this contract works — this one is just the
smallest faithful wrapper over the official npm runtime.

## Operational notes

- **Keep it private.** The service sees raw tool output; bind it to loopback
  (the default) or a private network, same trust domain as fleet itself.
- Long inputs are scanned with overlapping token-budget windows by the
  upstream runtime — nothing is silently truncated.
- Unlike Rampart's chat default, this service redacts CITY/STATE/ZIP too
  (`keepLabels: []`) — fleet redacts for a model context, not a chat UI.
  Edit `server.mjs` if your posture differs.
- ~25 ms per call on CPU after the one-time model load.
