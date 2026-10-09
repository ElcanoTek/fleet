# Task prompt size cap (250,000 bytes)

A scheduled task's prompt used to be capped at 100,000 characters. A 90-deal
Manifest create batch renders to roughly 234 KB, so the Operations Center
refused it even though the same batch is accepted by MOC. The cap is now
250,000 bytes, matching MOC.

## What shipped

- **Server.** `taskPromptMaxLength` in `internal/sched/handlers/handlers.go` is
  250000 and is measured in **bytes** (`len()` of the trimmed prompt), not
  characters. The error reads "prompt cannot exceed 250000 bytes". A 250 KB
  prompt still fits the 1 MiB JSON body cap (`MaxJSONBodySize`) with room for
  JSON escaping.
- **CLI replay.** `fleet sched dlq replay --prompt-file` applies the same bound
  (`replayPromptMaxLength` in `internal/admincli/sched_dlq.go`).
- **Web.** `validatePrompt` measures UTF-8 bytes with `TextEncoder` against
  250000. The Create Task prompt textarea no longer sets `maxLength`: that
  attribute counts UTF-16 code units, so it would truncate input before the byte
  validator could report anything, and for non-ASCII text the two units
  disagree. The validator is the single source of the limit.
- **Docs.** `docs/openapi.yaml` states the bound on `TaskCreate.prompt`; the
  Operations Center guide (and its web copy, via `make sync-guides`) tells users
  the limit.

## Deviations

- The unit changed from characters to bytes, deliberately: bytes is what the
  row, the body cap and MOC all bound. For ASCII text the numbers are equal;
  for multi-byte text the effective character limit is lower.

## Deferred

- No per-deployment configuration of the cap; it remains a compile-time
  constant that three call sites (handler, CLI, web) keep in step by hand.
- Other prompt-bearing surfaces (chat composer, prompt library) are unchanged.
