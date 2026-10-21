# ADR-0088: MCP workspace-file references: a 64 MiB whole-file cap, an 8 MiB per-call window, host-side hash verification, and one memory budget for every byte held

- **Status:** Accepted
- **Date:** 2026-10-21
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0066](0066-mcp-workspace-file-arguments.md) (its 2 MiB
  whole-file cap and its whole-file-read-per-chunk consequence)
- **Design note:** [`MCP-WORKSPACE-FILES.md`](../MCP-WORKSPACE-FILES.md)

## Context

ADR-0066 lets a model pass `{workspace_file, sha256, offset, length}` for an
MCP string property annotated `contentEncoding: base64`. It capped the whole
file at 2 MiB and re-read the whole file for every chunk.

MCP servers now take much larger uploads. Pages stages up to 64 MiB, in chunks
of up to 8 MiB, with MCP request bodies of up to 32 MB. Raising the cap means
designing for a hostile model. Such a model controls the sandbox: it can plant
modules, grow or swap files mid-read, and run many concurrent runs. It also
controls every argument of every call. Four things follow:

- **Integrity.** Nothing the sandbox reports can be trusted.
- **Cost.** Chunked uploads must stay linear.
- **Fleet memory.** Every byte Fleet or the broker child holds for a reference
  must be bounded.
- **Sandbox memory.** The FileOp helper must fit the smallest supported
  sandbox: 128 MiB, which is the kubernetes floor.

## Decision

1. **Caps.** A reference may name a file of up to 64 MiB
   (`maxMCPWorkspaceFileBytes`). One reference may carry at most 8 MiB of it
   (`maxMCPWorkspaceWindowBytes`). 8 MiB is Pages' largest chunk, and its
   base64 fits Pages' 32 MB request body.
   - Both numbers have a single definition in
     `internal/agentcore/mcp_file_arguments.go`.
   - The advertised description and the schema maxima are derived from them.
   - The server's `maxLength` still applies.
   - Every reference of a call is validated before anything is read.
2. **Fleet verifies the hash itself.** On the first reference to a file,
   `tools.WorkspaceFileReader` reads the whole file out of the sandbox and
   hashes it in Fleet, refusing a mismatch before any MCP call. Fleet uses
   nothing the helper says about the bytes. The read itself is bounded at
   every layer:
   - **Size probe.** A cheap probe (FileOp read with `MaxSize` 1, answered
     from fstat) sizes the read. The read's `MaxSize` is the probed size, so a
     lying probe cannot enlarge what is read.
   - **The helper streams.** It reads 768 KiB chunks, hashes each, base64s the
     part inside the window (carrying at most 2 bytes between chunks), and
     writes it straight to stdout. Its peak memory is O(chunk): about 20 MiB
     RSS for a 64 MiB read, and it runs under a 96 MiB address-space limit in
     tests. The response puts the data first and the status, size and hash
     after it. A failure found mid-read becomes an `ok:false` trailer.
   - **The helper counts what it reads.** It never reads past `MaxSize + 1`
     bytes, so a file grown mid-read is refused even if its size and mtime
     are restored before the after-read stat check.
   - **Fleet bounds the helper's output.** Every backend (podman, kubernetes,
     host) writes helper stdout into a sink allocated once at exactly
     base64(`MaxSize`) + 64 KiB. The sink refuses longer output and keeps
     draining the pipe. The payload is decoded straight out of the sink,
     never copied into a JSON string.
   - **The helper runs isolated.** It runs as `python3 -I` from `/` on every
     backend.
3. **Later windows are slices of the verified bytes.** Verified files are
   cached for up to 5 minutes, at most 2 unpinned files and 128 MiB. The cache
   key is the reader instance (one per run or turn binding), the workspace
   root, the path and the sha256. A window is a sub-slice of its file, with no
   copy. A key being read by one call is waited on by the others (holding
   nothing) rather than read twice. A file that fails verification is never
   cached.
4. **One budget charges every byte held for a reference.** The budget is
   384 MiB process-wide. A byte is charged before it is allocated and stays
   charged until it is released. The tally is
   `cached files + in-flight reads + window leases ≤ 384 MiB`. S is the file
   size, W the window, and E = base64(W):

   | Item | Charge | Held from → until |
   | --- | --- | --- |
   | Size probe output | within the 64 KiB response overhead | probe |
   | A file being read: sink (base64(S) + 64 KiB, allocated once) + decoded bytes (S) + the small response tail | `missCost(S) = 2.5·S + 192 KiB` | before the read → it settles |
   | A verified file, cached or pinned by a lease | `S` | settle → evicted (never while pinned) |
   | One window lease (cache hit **or** miss) | `windowCharge(W) = 10·E + 256 KiB` | before the windows are returned → after the dispatch has let go of the arguments |
   | · the window itself | 0 (a slice of the pinned file) | |
   | · parent: base64 argument string | E | |
   | · parent: broker frame encode (pooled `json` encodeState, growth) | ≤ 3E | |
   | · child: broker frame decode buffer (growth; the decoder is replaced after a large frame) | ≤ 3E | |
   | · child: decoded argument string | E | |
   | · child: JSON-RPC line encode (`mcp.EncodeJSONLine`, growth) + the encoded line | ≤ 4E | |

   - **Measured, not guessed.** `TestWorkspaceWindowDispatchAllocsWithinCharge`
     (`internal/mcpbroker`) counts every byte allocated while dispatching a
     1 MiB and an 8 MiB window through the real out-of-process broker and the
     real stdio line encoding, growth intermediates included. It measures
     about 8–9·E, an upper bound on what is live at once, and fails if the
     charge falls below that. It is skipped under `-race`, whose
     instrumentation inflates allocation. The in-process broker and the HTTP
     transport copy less.
   - **One admission per call.** A call's references are admitted together.
     References to the same file are grouped. A hit reserves its window
     charges and pins its file. A miss reserves its final cost
     (S + window charges). On top of that, the call reserves once the largest
     excess of any of its reads over that read's final cost, because a call
     reads its misses one at a time. Each read, as it settles, converts its
     bytes from reserved to cached (pinned). The call never holds a charge or
     a pin while it waits: if the call does not fit, it unpins, waits for a
     release (honouring cancellation and the MCP call's own deadline, which
     now also bounds resolution), and plans again. A call whose need exceeds
     the whole budget fails at once.
   - **What fits.**
     - Two 64 MiB files with an 8 MiB window each, in one call, need
       341.8 MiB.
     - One 64 MiB file being read costs 160.2 MiB.
     - An 8 MiB window of a 64 MiB file costs 64 MiB + 106.9 MiB, so two such
       calls on one file run at once and a third queues.
     - Pages' default 1 MiB chunk costs about 13.6 MiB.
5. **The lease is held until the dispatch provably lets go of the
   arguments.** `mcpTool.call` wraps the lease in an `mcp.ArgumentHold` and
   releases it after `CallMCP` returns, on every path including panics.
   Release puts the references back in `args`, which drops the base64.
   - **A cancelled out-of-process call.** `mcpbroker.Client` takes a
     retention (`mcp.RetainArguments`). It keeps the pending slot until the
     child's final response, or the connection's death, shows the child let
     go. Only then does the hold release.
   - **Cancellation is prompt.** `mcp.Server.callTool` now waits for the
     server's mutex with its context, so a cancelled call queued behind a slow
     one gives up at once. The stdio transport closes the stdin of a wedged
     server when a write is cancelled, so the goroutine blocked writing the
     request line exits and frees the line. The transport was already marked
     broken and is restarted.
   - **No teardown.** The broker connection is not torn down on a slow
     answer: it is the only one, and closing it would end every MCP call in
     the process with no restart. Keeping the charge keeps the bound honest
     instead.
6. Every other part of ADR-0066 still holds: path confinement, symlink
   refusal, governance before the read, broker authorization, and the journal
   and approval card recording the reference rather than the bytes.

## Enforcement

- `internal/agentcore`:
  - `TestMCPWorkspaceFileCap`: caps and the derived text and schema.
  - `TestMCPWorkspaceFileWindowsStayCharged`: 4 concurrent 8 MiB calls
    through a real sandbox never exceed the budget, and cache hits are
    charged.
  - `TestMCPWorkspaceFileLeaseReleasedOnEveryPath`: broker error,
    cancellation, panic, and a resolve that fails partway.
  - `TestMCPWorkspaceFileLeaseOutlivesRetainedDispatch`: the lease is kept
    while a dispatch retains the arguments.
  - `TestMCPWorkspaceFileTwoReferencesOneCall`.
- `internal/tools`:
  - `TestWorkspaceFileMultiReferenceAdmission`: two 64 MiB files with 8 MiB
    windows fit in one call; concurrent multi-reference calls in opposite
    orders do not deadlock; an impossible call fails fast; the same file
    twice is read once.
  - `TestWorkspaceFileReadPanicReleases`.
  - `TestWorkspaceFileLeases`.
  - `TestWorkspaceFileReaderBudget`.
  - `TestWorkspaceFileReaderConcurrentIdenticalMisses`.
  - `TestWorkspaceFileReaderRejectsForgedHelperDigest`.
  - `TestVerifiedWorkspaceFileCache`.
  - `TestWorkspaceFileReaderChunkedLargeFile`.
  - `TestWorkspaceFileReaderConfinement`.
- `internal/mcpbroker`:
  - `TestWorkspaceWindowDispatchAllocsWithinCharge`.
  - `TestCancelledCallRetainsArgumentsUntilChildAnswers`.
- `internal/mcp`:
  - `TestLockWithContextGivesUp`.
  - `TestArgumentHoldReleasesAfterCallerAndRetentions`.
- `internal/sandbox`:
  - `TestFileOp_ReadStreamsWithinSandboxMemory`: a 64 MiB read under
    `ulimit -v` 96 MiB, plus unaligned windows.
  - `TestFileOp_ReadMaxSizeCountsBytesRead`.
  - `TestFileOpSinkBoundsHelperOutput`.
  - `TestFileOp_ReadMaxSizeAndLargeWindow`.

## Consequences

- A chunked upload reads and hashes each file once per reference. The
  dispatched bytes are always the version the approved hash names.
- **The bound holds for reachable data.** Some memory sits outside it:
  - Garbage the Go collector has not yet reclaimed, up to the `GOGC` target.
  - `encoding/json`'s pooled encoder buffers, which may keep about 2·E after a
    call until GC.
  - An HTTP transport's write goroutine, for the moment after a cancelled
    request before its connection closes.
  - An MCP server's own process.
- **A child that never answers keeps its lease's charge.** This holds even
  past cancellation, until the connection dies. The cost is budget capacity,
  not untracked memory. With context-aware queueing and the transports
  honouring cancellation, the child answers promptly in practice.
- **Larger files cost more calls.** A 64 MiB file takes at least 8 calls. A
  single-call tool, such as Lens's `create_job`, is limited to 8 MiB.

## Alternatives considered

- **Trust the helper's digest and read only the window.** Rejected because the
  sandbox is model-writable.
- **Keep re-reading the whole file per chunk.** Rejected because it is
  quadratic.
- **Admit reads but not windows, or admit references one at a time.** These
  were earlier revisions. Rejected: they let base64 copies go uncharged, or
  allowed hold-and-wait deadlock.
- **Tear down the broker connection after a grace period.** Rejected because
  it kills all MCP for the process.
- **Stream the base64 from the sandbox into the MCP request.** This would
  remove the parent and child copies, but needs a broker wire change.
  Deferred.
