# ADR-0066: Hash-bound workspace references for MCP binary arguments

Status: accepted

## Context

A file already in the sandbox could reach an MCP upload only by being copied
through model output or by a shell HTTP request carrying a returned credential.
Output redaction and binary suppression make both unreliable, and credentials
must never enter the sandbox. Client-specific upload logic does not belong in
Fleet.

## Decision

A server's standard `contentEncoding: base64` string annotation opts that
argument into a generic file-reference alternative. The reference includes the
whole-file hash and exact raw range. Fleet reads within the active workspace
through sandbox FileOp after normal tool governance and before the same scoped
MCP broker call. Nil reader, path escape, size/range/hash mismatch fail closed.
The broker independently authorizes the original connector/tool as before.
The request journal and approval card bind the path, hash and range; changes
to file content cannot change the approved bytes. Only annotated binary fields
are eligible; every other field retains its original contract.

This extends the existing sandbox-file-to-MCP transport, adding no host file
or network exception to ADR-0036 and no new credential channel. The server defines each tool's side effects; file expansion does not add or
remove any commit or publication step.

## Consequences

Model context contains a bounded reference instead of generated binary data.
Sealed sandbox egress is compatible with brokered uploads. Clients lacking this
capability can still use the original string schema. A 2 MiB whole-file cap
bounds memory; reading and hashing the whole file for each chunk trades modest
I/O for exact identity without a mutable cross-turn cache. See
[MCP workspace files](../MCP-WORKSPACE-FILES.md) for shipped scope and limitations.
