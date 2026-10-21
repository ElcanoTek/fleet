# ADR-0085: MCP workspace-file references cap the whole file at 64 MiB, are hash-verified in Fleet once per reference, and serve each window from those verified bytes

- **Status:** Accepted
- **Date:** 2026-10-21
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0066](0066-mcp-workspace-file-arguments.md) (its 2 MiB
  whole-file cap and its whole-file-read-per-chunk consequence)
- **Design note:** [`MCP-WORKSPACE-FILES.md`](../MCP-WORKSPACE-FILES.md)

## Context

ADR-0066 lets a model pass `{workspace_file, sha256, offset, length}` for an
MCP string property annotated `contentEncoding: base64`. It capped the whole
file at 2 MiB, and Fleet read the whole file out of the sandbox for every chunk
so it could hash it host-side.

MCP servers now take much bigger uploads. Pages accepts staged uploads of
32 MiB by default and 64 MiB at most, in chunks of up to 8 MiB. With a 2 MiB
cap, Fleet was the limit. Raising the cap while keeping whole-file reads would
make a chunked upload quadratic: a 64 MiB file sent in 1 MiB chunks would move
64 whole copies (4 GiB) out of the sandbox, through JSON and base64, and into
Fleet's memory.

Reading only the window out of the sandbox and trusting a whole-file digest the
sandbox helper computed is not an option. The sandbox is model-writable: a
planted module or a compromised interpreter could report the approved hash for
altered bytes. The approved hash has to be checked by Fleet over the bytes it
actually sends.

## Decision

1. **The whole-file cap is 64 MiB.** `maxMCPWorkspaceFileBytes` in
   `internal/agentcore/mcp_file_arguments.go` is the only copy of the number.
   The advertised description text and the schema maxima are derived from it.
2. **Fleet hashes the whole file itself, once per reference.** On the first
   reference to a file, `tools.WorkspaceFileReader` reads the whole file out of
   the sandbox through FileOp with `MaxSize = cap`. The helper refuses a file
   over the cap from its size, before reading any of it. It also counts the
   bytes it actually reads, and never reads more than `MaxSize + 1`, so a file
   grown mid-read (and restored before the after-read stat check) is refused
   rather than buffered. On Fleet's side, every backend (podman, kubernetes,
   host) buffers at most the base64 of `MaxSize` (or `Limit`) plus 64 KiB of
   helper output for such a read. A longer response is discarded and refused,
   so even a helper that is not the script Fleet sent cannot exhaust Fleet's
   memory. Fleet then hashes the
   bytes and refuses a mismatch before any MCP call. Nothing the helper reports
   about the bytes, size or digest, is used.
3. **Later windows are sliced from the verified bytes.** The verified bytes go
   into an LRU that holds at most 2 files and 128 MiB process-wide, with a
   5-minute TTL. The key is the reader instance (one per run or turn binding to
   a sandbox), the workspace root, the path and the sha256. Each window of the
   same reference is a slice of those bytes. On a miss (eviction, expiry, a
   new turn), Fleet re-reads and re-verifies. A file that fails verification
   is never cached.
4. **The helper runs in Python isolated mode from `/`** on every backend
   (podman `exec --workdir / python3 -I -c`, kubernetes `cd / && python3 -I`,
   the host test executor `python3 -I -c`). This is defence in depth: a
   workspace file cannot shadow a stdlib module the helper imports.
5. Every other part of ADR-0066 still holds: the path must stay inside the
   active workspace, symlinks are rejected, an exact range and the server's
   `maxLength` are enforced before reading, governance runs before the read,
   the broker authorizes the call, and the journal and approval card record the
   reference.

## Enforcement

- `TestMCPWorkspaceFileCap` (`internal/agentcore`): pins the cap at 64 MiB and
  checks that the description and schema carry it. A 3 MiB file is accepted
  and its window is delivered exactly. A file of 64 MiB + 1 byte is refused
  with no MCP call. A range past the cap never reaches the reader.
- `TestWorkspaceFileReaderRejectsForgedHelperDigest` (`internal/tools`): a
  helper that returns altered bytes while claiming the approved hash is
  refused every time, and nothing is cached.
- `TestVerifiedWorkspaceFileCache`: covers a hit (several windows, one read),
  a miss across reader instances, LRU eviction by entry and byte bounds, TTL
  expiry, and an over-budget file that is never cached.
- `TestWorkspaceFileReaderChunkedLargeFile`: a 3 MiB file sent in 1 MiB
  windows through the real sandbox is byte-exact, even when the file is
  rewritten mid-upload.
- `TestWorkspaceFileReaderConfinement`: the existing path and root confinement
  cases, plus hash refusal and cap refusal.
- `TestFileOp_ReadMaxSizeAndLargeWindow` (`internal/sandbox`): `MaxSize`
  refusal, and a multi-MiB read that comes back byte-exact from the real
  `fileops.py`.
- `TestFileOp_ReadMaxSizeCountsBytesRead`: a file that never reaches EOF is
  refused after `MaxSize + 1` bytes, for whole and windowed reads.
- `TestFileOpSinkBoundsHelperOutput`: the Go-side output bound, and the
  refusal of a flooding helper with buffering stopped at the bound.

## Consequences

- A chunked upload reads and hashes the file once, not once per chunk, as long
  as its chunks arrive within the TTL and the cache is not churned by other
  large references. A miss costs one more whole-file read. The worst case is
  the pre-cache behaviour, bounded by the cap.
- The dispatched bytes are always the version the approved hash names. A file
  rewritten mid-upload keeps sending the verified version until the entry
  leaves the cache, and after that the reference fails its hash check.
- Host memory: the cache holds at most 128 MiB. A miss briefly holds one file
  of up to 64 MiB plus its base64 and decode copies. The helper writes the
  base64 payload straight to stdout instead of copying it again into a dumped
  JSON string.
- No other limit on the path was in the way. The broker's JSON framing, the
  stdio and HTTP MCP request bodies, and the FileOp exec output have no
  request-size cap. The existing caps apply only to MCP *responses*.

## Alternatives considered

- **Keep reading the whole file per chunk at the larger cap.** Rejected
  because a chunked upload becomes quadratic in transfer and host memory.
- **Read only the window and trust the helper's whole-file digest.** This was
  the first draft of this ADR. Rejected because the digest is computed in
  model-writable territory, so it cannot bind the approved hash to the bytes
  Fleet sends.
- **A cross-turn cache keyed only on path.** Rejected for the reason ADR-0066
  gave: a cache keyed on a path has to be invalidated correctly against a
  sandbox that can rewrite the file at any time. Keying on the verified hash
  and the reader instance removes the need to invalidate anything.
