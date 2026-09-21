# Verifier sees the run's final response

The scheduled end-of-run verifier (`internal/agent/verifier.go`,
`runEndOfRunVerifier`) used to receive only the original task text and the
bounded tool-execution summary. A task step phrased "Report/summarize/state X"
is fulfilled in the run's closing assistant message, not in a tool call, so the
verifier could never see that content: it re-demanded the report on all three
checks, the run dead-lettered as `ErrCompletionUnverified`, and fully-completed
work (page published and verified live, outcome written in prose) sat in the
DLQ. fleet.elcanotek.com hit exactly this on 2026-09-19 and 2026-09-21.

## What shipped

- `runEndOfRunVerifier(ctx, task, finalResponse, records)` takes the run's
  latest assistant text as a third input. The user prompt gains a clearly
  delimited `FINAL RESPONSE (the agent's closing message, ...)` section after
  the tool-executions block.
- The section is bounded by `verifierMaxFinalResponseChars` (8,000) with
  head+tail truncation (the opening summary and the closing details are both
  evidence), mirroring `truncateTaskForVerifier`. Empty/whitespace text becomes
  the explicit `(no final response text)` marker so a genuinely missing report
  stays flaggable — a blank section would read as "nothing to check".
- The system prompt gains one rule: a requirement to report/summarize/state/
  describe something **in the run's own output** is satisfied when the FINAL
  RESPONSE contains that content; only deliverables that require a tool call
  (email send, deal creation, page write, file upload, ...) still demand one.
  Every existing rule is intact, and the section is labelled evidence, never
  instructions — the same untrusted-evidence stance as tool fields.
- **Gate 1 (`CanFinish`) and Gate 2 (phone-a-friend) read the final text from
  the run observer's live text tracker** (`scheduledObserver.latestText`),
  falling back to `latestAssistantText(logSession)`. This is the part the
  one-line reading of the fix misses: the core streams assistant text only as
  `text.delta` events and the driver persists the completed response into the
  session log only AFTER `agentcore.Run` returns — so at `CanFinish` time the
  session does not contain the closing message and `latestAssistantText` alone
  would return `""` (or a stale pre-audit draft). The tracker accumulates the
  current delta burst in memory (any non-delta event closes it; `text.replace`
  is authoritative), changes no persisted-log shape, and feeds both gates the
  run's actual closing message. Gate 2 previously passed an empty answer to the
  reviewer on every scheduled run — same blind spot, fixed by the same helper.

## Tests

- `TestRunEndOfRunVerifierIncludesFinalResponse` — the prompt the verifier
  model receives contains the FINAL RESPONSE section with the latest assistant
  text.
- `TestTruncateFinalResponseForVerifier` — bound, head+tail kept, cut marked;
  empty → explicit marker.
- `TestRunEndOfRunVerifierEmptyFinalResponseMarked` — empty text reaches the
  verifier as `(no final response text)`.
- `TestScheduledObserverTracksLatestTextForFinishGates` — burst accumulation,
  close-on-non-delta, `text.replace` authority.
- `TestScheduledCompletionAfterCommittedWrite` (extended) — the scheduled
  driver end-to-end: Gate 1's verifier prompt carries the run's closing message
  across the real core/observer seam, in both the clean and the
  exhausted-verification paths.

## Deviations

- The brief said "pass `latestAssistantText` into `runEndOfRunVerifier`";
  shipped as `latestRunText()`, the tracker-first helper described above,
  because the session-only reading is a no-op at the gate seam (proven by the
  extended driver test failing before the tracker existed). The fallback keeps
  the session path for any policy built without a tracker.
- Gate 2's answer source changed as a consequence (same helper). It is the
  behavior the reviewer was always documented to have ("the answer/work the
  reviewer critiques") and the feature is off unless an operator enables
  phone-a-friend.

## Deferred

- No change to what makes a run fail: the verifier's demands for tool-backed
  deliverables are untouched; this only adds evidence it was blind to.
- Interactive (chat) runs have no end-of-run verifier — unchanged.
- The persisted captain's log shape is unchanged (drafts of failed enforcement
  rounds are not written to the session; only the completed response lands,
  as before).
