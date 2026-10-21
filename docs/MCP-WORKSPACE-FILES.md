# MCP binary arguments from workspace files

Fleet accepts an exact file reference for an MCP input property whose server
schema declares `type: string` and `contentEncoding: base64`. This is opt-in
per property, not a guess based on a connector or parameter name. Fleet adds
an alternative object schema only when a sandbox reader is bound:

```json
{"workspace_file":"report.json","sha256":"<64 lowercase hex characters>","offset":0,"length":49152}
```

`sha256` identifies the whole file. Offset and length select raw bytes. The
whole file is capped at 64 MiB (67108864 bytes, the largest staged upload a
Pages-class server accepts), each encoded chunk must fit the server's
`maxLength`, and an altered file fails before any MCP call. Only the requested
window leaves the sandbox: the FileOp helper refuses an over-cap file by its
size, streams the whole file through SHA-256, and returns just the window with
the whole-file hash, so a chunked upload moves each byte once rather than the
whole file per chunk ([ADR-0085](adr/0085-mcp-workspace-file-cap-and-windowed-reads.md)).
The helper still hashes the whole file on every call, which is up to 64 MiB of
sequential I/O inside the sandbox per chunk. References must
stay in the current conversation or scheduled workspace. Fleet reads through
sandbox FileOp, base64-encodes bytes in fixed broker adapter code, and calls
the original tool through its usual scoped broker. Model input, approvals and
journals retain the reference, not the large binary value. Tool outputs still
cross redaction, screening and size limits.

This works in interactive and scheduled runs, including sealed sandbox egress:
only the already-authorized MCP broker transports bytes. It creates no generic
HTTP uploader, exposes no credential to the model or sandbox, and changes no
approval or connector allowlist. Existing base64 string calls remain valid.
Client bundles/external servers own upload tool names, sequence, chunk size,
commit, optimistic concurrency and post-publication checks. A file reference
changes transport only, not the called tool's side effects. Retries use the server's existing idempotence contract.

Shipped: top-level base64 string properties, bounded whole-file hash checks,
windowed range reads, range encoding, sandbox confinement, and shared runtime
dispatch. A single call whose server sets no `maxLength` may still ask for the
whole 64 MiB in one window; that call holds the window and its base64 form in
memory. Deliberately deferred: files over 64 MiB, streaming base64 straight
from the sandbox into the MCP request, automatic multi-call upload orchestration,
JSON/text argument expansion and generic credential-bearing HTTP transfers.
