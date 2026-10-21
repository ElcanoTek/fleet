# ADR-0087: MCP workspace-file references: a 64 MiB whole-file cap, an 8 MiB per-call window, host-side hash verification, and one memory budget for every byte held

- **Status:** Accepted
- **Date:** 2026-10-21
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0066](0066-mcp-workspace-file-arguments.md) (its 2 MiB
  whole-file cap and its whole-file-read-per-chunk consequence)
- **Design note:** [`MCP-WORKSPACE-FILES.md`](../MCP-WORKSPACE-FILES.md)

## Context

ADR-0066 lets a model pass `{workspace_file, sha256, offset, length}` for an
MCP string property annotated `contentEncoding: base64`. It capped the whole
file at 2 MiB, and Fleet read and hashed the whole file for every chunk.

MCP servers now take much larger uploads. Pages stages up to 64 MiB, in chunks
of up to 8 MiB, with MCP request bodies of up to 32 MB. Raising the cap exposes
three problems.

- **Integrity.** The sandbox is model-writable. A digest computed inside it,
  for example by a planted `hashlib.py`, cannot bind the approved hash to the
  bytes Fleet sends.
- **Cost.** Re-reading the whole file for every chunk makes a chunked upload
  quadratic.
- **Memory.** A large file, the base64 of each window, and every whole-request
  copy the dispatch path makes of that base64 all live in Fleet. Concurrent
  runs can multiply them past anything advertised.

## Decision

1. **Caps.** A reference may name a file of up to 64 MiB
   (`maxMCPWorkspaceFileBytes`). One call may carry at most 8 MiB of it
   (`maxMCPWorkspaceWindowBytes`). 8 MiB is Pages' largest chunk, and its
   base64 fits Pages' 32 MB request body. Both numbers have a single definition
   in `internal/agentcore/mcp_file_arguments.go`, and the advertised
   description and schema maxima are derived from them. The server's own
   `maxLength` still applies, and every cap is checked before anything is
   read.
2. **Fleet verifies the hash itself.** On the first reference to a file,
   `tools.WorkspaceFileReader` reads the whole file out of the sandbox and
   hashes it in Fleet. It refuses a mismatch before any MCP call. It uses
   nothing the helper says about the bytes.
   - The FileOp helper refuses a file over its `MaxSize` from fstat.
   - The helper also counts the bytes it actually reads and never reads past
     `MaxSize + 1`, so a file grown during the read is refused.
   - Every backend (podman, kubernetes, host) stores at most base64(`MaxSize`
     or `Limit`) + 64 KiB of helper stdout in a buffer that grows no further
     than that, and refuses longer output.
   - The helper runs as `python3 -I` from `/` on every backend.
3. **Later windows are slices of the verified bytes.** Verified files are
   cached for up to 5 minutes, at most 2 unpinned files and 128 MiB. The cache
   key is the reader instance (one per run or turn binding), the workspace
   root, the path and the sha256. A window is a sub-slice of the cached file,
   so there is no copy. Concurrent misses on the same key share one read. A
   file that fails verification is never cached.
4. **One budget charges every byte Fleet holds for a reference.** The budget
   is 384 MiB process-wide. A byte is charged before it is allocated and stays
   charged until it is released. The tally is
   `cached files + in-flight reads + window leases ≤ 384 MiB`.

   | Item | Charge | Held from → until |
   | --- | --- | --- |
   | Size probe output (FileOp read, `MaxSize` 1) | inside the 64 KiB response overhead | probe call |
   | Miss read: FileOp output buffer (≤ base64(S) + 64 KiB) + its JSON-decoded string (same) + decoded bytes (S) | `4·S + 128 KiB` | before the read → read settles |
   | Verified file (cached, or pinned by a lease) | `S` | read settles → evicted (only when unpinned) |
   | Window W leased to one call (cache hit **or** miss), with E = base64(W): | `WorkspaceWindowCharge(W) = 12·E + 256 KiB` | before the window is returned → after the outbound call returns |
   | · the window itself | 0 (a slice of the pinned file) | |
   | · parent: base64 argument string (`resolveFileReferences`) | E | |
   | · parent: broker frame encode (pooled `json` encodeState, growth) | ≤ 3E | |
   | · child: broker frame decode buffer (growth; the decoder is replaced after a large frame, so the buffer is not retained) | ≤ 3E | |
   | · child: decoded argument string | E | |
   | · child: JSON-RPC line encode (`mcp.EncodeJSONLine`, growth) + the encoded line | ≤ 4E | |

   - **Measured, not guessed.** The window charge is calibrated by
     `TestWorkspaceWindowDispatchAllocsWithinCharge` (`internal/mcpbroker`).
     It counts every byte allocated while dispatching a 1 MiB and an 8 MiB
     window through the real out-of-process broker (client encode, server
     decode) and the real stdio line encoding, growth intermediates included.
     The measured total, about 9·E, is an upper bound on what is live at once,
     and the test fails if the charge falls below it. The in-process broker and
     the HTTP transport make fewer copies.
   - **No hold-and-wait.** A miss reserves `max(4·S + 128 KiB, S + windowCharge)`
     up front. When the read settles, that reservation converts into the pinned
     file plus the lease without waiting again. A hit reserves its window
     charge and pins the file in one step. Waiting therefore never happens
     while part of a request's charge is held.
   - **Room is made in a fixed order.** First, unpinned files are evicted,
     least recently used first. Then the request waits for releases,
     honouring cancellation. A request whose charge exceeds the whole budget
     fails at once.
   - **Releasing a lease.** Releasing is idempotent. `mcpTool.call` defers it
     right after resolving, so the lease is released after `CallMCP` returns on
     every path: success, error, cancellation and panic. Releasing also puts
     the reference back in `args`, which drops the base64 string. A resolve
     that fails partway releases what it already acquired.
   - **What fits.** A 64 MiB file being read reserves 256 MiB + 128 KiB. Steady
     state for an 8 MiB window of a 64 MiB file is 64 MiB + 128.3 MiB. So two
     such windows run at once on one file, and further calls queue. Pages'
     default 1 MiB chunk costs about 16.3 MiB per call.
5. Every other part of ADR-0066 still holds: path confinement, symlink
   refusal, governance before the read, broker authorization, and the journal
   and approval card recording the reference rather than the bytes.

## Enforcement

- `internal/agentcore`:
  - `TestMCPWorkspaceFileCap` covers both caps, the derived text and schema, a
    3 MiB file being accepted, 64 MiB + 1 being refused, and an over-wide
    window being refused before any read.
  - `TestMCPWorkspaceFileWindowsStayCharged` (real sandbox, slow broker):
    4 concurrent 8 MiB calls never exceed the budget, at most the number the
    budget admits are in the broker at once, and every in-broker sample has at
    least `file + n·windowCharge` charged.
  - `TestMCPWorkspaceFileLeaseReleasedOnEveryPath` covers broker error,
    cancellation while blocked, broker panic, and a partial resolve failure.
- `internal/tools`:
  - `TestWorkspaceFileLeases`: hits are charged, a lease that does not fit
    waits and honours cancellation, release is idempotent, and a pinned file
    is never evicted.
  - `TestWorkspaceFileReaderBudget`: the peak stays within the budget, an
    oversize request fails fast, and waiting honours cancellation.
  - `TestWorkspaceFileReaderConcurrentIdenticalMisses`: concurrent identical
    misses share one read.
  - `TestWorkspaceFileReaderRejectsForgedHelperDigest`: a forged helper digest
    is refused.
  - `TestVerifiedWorkspaceFileCache`: cache hits, misses, LRU eviction and TTL
    expiry.
  - `TestWorkspaceFileReaderChunkedLargeFile`: a chunked upload is byte-exact.
  - `TestWorkspaceFileReaderConfinement`: confinement.
- `internal/mcpbroker`: `TestWorkspaceWindowDispatchAllocsWithinCharge`, the
  charge calibration.
- `internal/sandbox`:
  - `TestFileOp_ReadMaxSizeCountsBytesRead`: a file that never reaches EOF is
    refused after `MaxSize + 1` bytes.
  - `TestFileOpSinkBoundsHelperOutput`: the output bound, buffer capacity
    within the bound, and refusal of a flood.
  - `TestFileOp_ReadMaxSizeAndLargeWindow`.

## Consequences

- A chunked upload reads and hashes the file once per reference rather than
  once per chunk. The dispatched bytes are always the version the approved
  hash names, even if the file is rewritten mid-upload.
- **The bound holds for reachable data during a call.** Three things sit
  outside it:
  - Go's collector may let freed garbage linger up to the `GOGC` target.
  - `encoding/json`'s pooled encoder buffers may keep up to about 2·E after a
    call, until the next GC cycles reclaim them.
  - An MCP server's own process is not counted.
- Calls past what the budget admits queue rather than fail. A single call can
  only fail for budget reasons when its own charge can never fit, which the
  caps rule out for any valid reference.

## Alternatives considered

- **Trust the helper's digest and read only the window.** Rejected because the
  sandbox is model-writable.
- **Keep the whole-file read per chunk.** Rejected because it is quadratic.
- **Admit reads but not windows.** This was an earlier revision. Rejected
  because the window's base64 and its dispatch copies are the larger, longer
  lived part.
- **Stream the base64 from the sandbox into the MCP request.** This would
  avoid the parent and child copies, but needs a broker wire change. Deferred.
