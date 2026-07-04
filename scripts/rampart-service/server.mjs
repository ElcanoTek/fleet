// Rampart PII detection service — the reference implementation of fleet's
// detection-service contract (docs/PII-REDACTION.md), wrapping the official
// @nationaldesignstudio/rampart runtime (MiniLM token-classification ONNX,
// 17 PII entity types) on Node's CPU ONNX Runtime.
//
// Contract (what fleet's RampartRedactor speaks):
//
//   POST /v1/redact   {"text": "..."}
//   200               {"text": "<redacted with stable placeholders>",
//                      "findings": [{"kind": "given_name", "count": 1}, ...]}
//
// Design notes:
//   - The classifier (model + tokenizer) loads ONCE (~1s); each request gets a
//     fresh ChatGuard sharing it, so placeholder numbering is stable within a
//     request and no session table grows across requests (~25ms per call).
//   - detectNer scans arbitrarily long input with overlapping token-budget
//     windows, so large tool outputs are handled without truncation.
//   - keepLabels is emptied: fleet redacts server-side for a model context,
//     so CITY/STATE/ZIP (which Rampart's chat default preserves) are redacted
//     here too. Adjust below if your posture differs.
//   - Bind to loopback (default) — the service sees raw tool output; treat it
//     like fleet itself and keep it on the same host or a private network.
//
// Run:  npm install && npm start        (defaults to 127.0.0.1:8787)
// Env:  RAMPART_ADDR=127.0.0.1:8787  RAMPART_MAX_BODY_BYTES=1048576

import http from "node:http";
import { ChatGuard, detectNer, loadNerClassifier } from "@nationaldesignstudio/rampart";

const [host, port] = (process.env.RAMPART_ADDR ?? "127.0.0.1:8787").split(":");
const maxBody = Number(process.env.RAMPART_MAX_BODY_BYTES ?? 1048576);

console.log("rampart-service: loading model (first run downloads ~15 MB from Hugging Face)…");
const t0 = Date.now();
const classifier = await loadNerClassifier({ device: "cpu" });
console.log(`rampart-service: model ready in ${Date.now() - t0} ms`);

// findingsFrom counts DISTINCT placeholders per label: [GIVEN_NAME_1] and
// [GIVEN_NAME_2] are two findings of kind given_name; a re-mention of _1 is
// still one entity.
function findingsFrom(placeholders) {
  const counts = new Map();
  for (const p of new Set(placeholders)) {
    const m = /^\[([A-Z_]+?)_(\d+)\]$/.exec(p);
    if (!m) continue;
    const kind = m[1].toLowerCase();
    counts.set(kind, (counts.get(kind) ?? 0) + 1);
  }
  return [...counts.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([kind, count]) => ({ kind, count }));
}

async function redact(text) {
  const guard = new ChatGuard({
    ner: (t) => detectNer(t, classifier),
    keepLabels: [], // redact everything, including city/state/zip
  });
  const { text: redacted, placeholders } = await guard.protect(text);
  return { text: redacted, findings: findingsFrom(placeholders ?? []) };
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    req.on("data", (c) => {
      size += c.length;
      if (size > maxBody) {
        reject(Object.assign(new Error("body too large"), { status: 413 }));
        req.destroy();
        return;
      }
      chunks.push(c);
    });
    req.on("end", () => resolve(Buffer.concat(chunks).toString("utf8")));
    req.on("error", reject);
  });
}

const server = http.createServer(async (req, res) => {
  if (req.method === "GET" && req.url === "/healthz") {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ ok: true }));
    return;
  }
  if (req.method !== "POST" || req.url !== "/v1/redact") {
    res.writeHead(404).end();
    return;
  }
  try {
    const body = JSON.parse(await readBody(req));
    if (typeof body?.text !== "string") {
      res.writeHead(400, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ error: "want {\"text\": string}" }));
      return;
    }
    const out = await redact(body.text);
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify(out));
  } catch (err) {
    // Never echo the text back in an error.
    res.writeHead(err.status ?? 500, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ error: err.status ? err.message : "redaction failed" }));
  }
});

server.listen(Number(port), host, () => {
  console.log(`rampart-service: listening on http://${host}:${port}/v1/redact`);
});

// Exit promptly on SIGTERM/SIGINT so systemd/podman restarts don't wait out a
// kill timeout.
for (const sig of ["SIGTERM", "SIGINT"]) {
  process.on(sig, () => {
    console.log(`rampart-service: ${sig}, shutting down`);
    server.close(() => process.exit(0));
    setTimeout(() => process.exit(0), 2000).unref();
  });
}
