# ADR-0085: MCP workspace-file references cap the whole file at 64 MiB and read only the requested window

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

## Decision

1. **The whole-file cap is 64 MiB.** `maxMCPWorkspaceFileBytes` in
   `internal/agentcore/mcp_file_arguments.go` is the only copy of the number.
   The advertised description text and the schema maxima are derived from it.
2. **Only the requested window leaves the sandbox.** The reader
   (`tools.WorkspaceFileReader`) asks the sandbox FileOp helper for
   `[offset, offset+length)` and for `MaxSize = cap`. The helper refuses a file
   over the cap based on its size, before reading any of it. Otherwise it streams
   the whole file through SHA-256 in 1 MiB reads, keeps only the window, and
   refuses a file that changed while it was being read. It then returns the
   window together with the whole file's size and hash.
3. **The sha256 binding still covers the whole file.** Fleet refuses the call,
   with no MCP request made, in any of these cases: the reported whole-file
   hash differs from the reference, the window is shorter than `length`, or the
   range runs past the file or the cap. When the window is the whole file,
   Fleet also hashes the bytes itself.
4. Every other part of ADR-0066 still holds: the path must stay inside the
   active workspace, symlinks are rejected, an exact range and the server's
   `maxLength` are enforced before reading, governance runs before the read,
   the broker authorizes the call, and the journal and approval card record the
   reference.

## Enforcement

- `TestMCPWorkspaceFileCap` (`internal/agentcore`): pins the cap at 64 MiB and
  checks that the description and schema carry it. A 3 MiB file is accepted
  and its window is delivered exactly. A file of 64 MiB + 1 byte is refused
  with no MCP call. A range past the cap never reaches the reader.
- `TestMCPWorkspaceFileHelperHashMismatch`: when a whole-file window does not
  hash to the reported hash, the call is refused.
- `TestWorkspaceFileReaderConfinement` (`internal/tools`): covers windowed
  reads, the whole-file hash on a window, cap refusal on a 1-byte window, and
  range validation, alongside the existing confinement cases.
- `TestFileOp_ReadMaxSizeAndLargeWindow` (`internal/sandbox`): `MaxSize`
  refusal, and a multi-MiB window that comes back byte-exact from the real
  `fileops.py`.

## Consequences

- Each chunk call moves only its own bytes. The sandbox helper still hashes
  the whole file on every chunk, which costs up to 64 MiB of sequential I/O
  inside the sandbox per call. ADR-0066 chose this trade for exact identity
  without a mutable cross-turn cache, and it is kept.
- For a sub-range, the whole-file hash is computed by the helper inside the
  sandbox. Fleet does not re-derive it. This does not change the trust model:
  the window's bytes also come from the sandbox, so a compromised helper could
  already return any bytes. The hash binds the model's reference to one version
  of the file. It was never a defence against the sandbox itself.
- Memory scales with the window, not the cap. The worst case is a server with
  no `maxLength` asked for all 64 MiB in one window. That costs one 64 MiB
  read, plus its base64 form in the helper (written straight to stdout, not
  copied again into a dumped JSON string), plus the usual decode and
  re-encode copies in Fleet. Pages-class servers bound each chunk, for example
  to 8 MiB.
- No other limit on the path was in the way. The broker's JSON framing, the
  stdio and HTTP MCP request bodies, and the FileOp exec output have no
  request-size cap. The existing caps apply only to MCP *responses*.

## Alternatives considered

- **Keep reading the whole file per chunk at the larger cap.** Rejected
  because a chunked upload becomes quadratic in transfer and host memory.
- **Cache the hash across calls.** Rejected for the reason in ADR-0066: a
  mutable cache keyed on a path has to be invalidated correctly against a
  sandbox that can rewrite the file at any time.
- **Stream the base64 straight from the helper into the MCP request.** This
  would save the decode and re-encode copies. Rejected as needless complexity
  at a 64 MiB worst case.
