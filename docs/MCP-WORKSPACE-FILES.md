# MCP binary arguments from workspace files

Fleet accepts an exact file reference for an MCP input property whose server
schema declares `type: string` and `contentEncoding: base64`. This is opt-in
per property, not a guess based on a connector or parameter name. Fleet adds
an alternative object schema only when a sandbox reader is bound:

```json
{"workspace_file":"report.json","sha256":"<64 lowercase hex characters>","offset":0,"length":49152}
```

`sha256` identifies the whole file, and offset and length select raw bytes.

- **Size limits.** The whole file is capped at 64 MiB (67108864 bytes), and
  one call may carry at most 8 MiB (8388608 bytes) of it. Each encoded chunk
  must also fit the server's own `maxLength`, and a larger file goes as
  several calls. An altered file fails before any MCP call.
- **Hash checking.** Fleet hashes the whole file itself and never trusts a
  digest computed inside the model-writable sandbox. It reads the file once
  per reference. Later windows are slices of those verified bytes, kept for
  the run or turn in a bounded cache (two unpinned files, 128 MiB, five
  minutes), so a chunked upload does not re-read the file for each chunk. A
  file rewritten mid-upload keeps sending the verified version until the cache
  entry ages out; after that the reference fails its hash check.
- **Reading safely.** The size cap is enforced on the bytes actually read, so
  a file that grows during the read is refused. Fleet buffers no more helper
  output than such a read can produce.

Every byte Fleet holds for a reference is charged to one 384 MiB process-wide
budget, from before it is allocated until it is released
([ADR-0088](adr/0088-mcp-workspace-file-cap-and-windowed-reads.md)). S is the
file size, W the window, and E = base64(W):

| Held | Charge | Until |
| --- | --- | --- |
| A file being read (the FileOp output buffer, allocated once, and the decoded bytes) | 2.5·S + 192 KiB | the read settles |
| A verified file, cached or pinned by a call using it | S | evicted, which can only happen while unpinned |
| One call's window, on a cache hit or a miss: its base64 and every whole-request copy the broker and JSON-RPC encoding make | 10·E + 256 KiB, about 9·E measured | the dispatch lets go of the arguments: CallMCP returned and, for a cancelled out-of-process call, the broker child answered |

- **One admission per call.** All references of a call are admitted
  together, so a call never holds some windows while it waits for others; a
  call that could never fit fails at once. Two 64 MiB files with 8 MiB
  windows in one call need about 342 MiB.
- **Identical references** in flight share one read.
- **Admission order.** A request first evicts unpinned files, then queues,
  honouring cancellation. One that could never fit fails at once.
- **Pages examples.** A 1 MiB Pages chunk costs about 14 MiB while its call is
  in flight. An 8 MiB chunk of a 64 MiB file costs 64 MiB + 107 MiB, so two
  such calls run at once and a third waits.
- **In the sandbox.** The FileOp helper streams a read (hash, base64 and
  write in fixed-size chunks), so a 64 MiB read uses about 20 MiB and fits the
  smallest supported sandbox.
- **Outside the bound.** Garbage the Go collector has not yet reclaimed and
  `encoding/json`'s pooled buffers after a call are not counted.

References must
stay in the current conversation or scheduled workspace. Fleet reads through
sandbox FileOp, base64-encodes bytes in fixed broker adapter code, and calls
the original tool through its usual scoped broker. Model input, approvals and
journals retain the reference, not the large binary value. Tool outputs still
cross redaction, screening and size limits.

This works in interactive and scheduled runs, including sealed sandbox egress:
only the already-authorized MCP broker transports bytes. A critical tool staged
with a reference keeps the reference on its approval card; the file is read
when the person clicks Approve, through a fresh sandbox taken under the
conversation's network posture with that conversation's workspace bound, so
the same confinement, whole-file hash check, caps and memory budget apply. A
file changed or removed after staging fails the approval with nothing sent. It creates no generic
HTTP uploader, exposes no credential to the model or sandbox, and changes no
approval or connector allowlist. Existing base64 string calls remain valid.
Client bundles/external servers own upload tool names, sequence, chunk size,
commit, optimistic concurrency and post-publication checks. A file reference
changes transport only, not the called tool's side effects. Retries use the server's existing idempotence contract.

Shipped: top-level base64 string properties, bounded whole-file hash checks,
host-verified reads with a bounded per-run cache, range encoding, sandbox confinement, and shared runtime
dispatch. Deliberately deferred: files over 64 MiB, windows over 8 MiB per call,
streaming base64 straight from the sandbox into the MCP request, automatic multi-call upload orchestration,
JSON/text argument expansion and generic credential-bearing HTTP transfers.
