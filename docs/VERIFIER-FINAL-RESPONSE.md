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
- **Both finish gates read the answer through the core's round-text seam**
  (`agentcore.RoundFinalTextReceiver`): before each `CanFinish` consultation,
  `agentcore.Run` hands the policy the closing assistant text of the round
  that just ended — computed exactly as the result's `FinalText` (per-round
  stream accumulation, the completed response preferred). The session log
  cannot supply this: the driver persists the completed response only after
  `Run` returns, so at `CanFinish` time `latestAssistantText(session)` is
  empty. Because the value is computed per round, a repair round that produces
  no text yields `""` and the verifier sees the explicit marker — never a
  previous round's rejected draft combined with the current round's fresh tool
  evidence (see the review fix below).
- **A reviewer-forced repair invalidates the verifier's approval.** Gate 2
  (phone-a-friend) is single-shot per run, but when its issue list forces a
  repair round, `verified` is reset: the revised answer has not been verified,
  so the next `CanFinish` re-runs Gate 1 against the repaired round's own
  closing text. The re-check counts against the SAME three-call cap — Gate 1
  refuses a fourth verification and routes through the exhaustion path
  (`ErrCompletionUnverified`, detail naming the reviewer-forced repair), so
  exhaustion never grants success. Pre-fix the repaired answer shipped
  unverified.
- **Scope note (structured-output tasks):** the gates judge the round's
  free-form closing text. The terminal structured-output phase
  (`completeRun`) runs AFTER the finish gates and replaces the persisted
  `FinalText` with the schema-valid JSON; that JSON is validated against the
  schema, not re-judged by the gates. Running the gates against the structured
  value would mean moving a paid terminal generation inside the finish gate
  with undefined repair semantics — not a small change, so the behavior is
  documented rather than forced. The draft is in any case the text that
  carries the prose a "Report X" step demands.

## Review fix: the text must belong to the CURRENT round

Codex P1 on the first revision of this change: that revision tracked the run's
text in the observer (accumulating `text.delta` bursts), and a repair round
that produced no text left the tracker holding the PREVIOUS round's rejected
draft. Since the loop still consults `CanFinish` after a textless repair round,
the verifier could combine that stale report with the new tool evidence and
approve — while `completeRun` persisted the textless round's empty `FinalText`:
the task marked successful without its closing report.

Fix: the observer tracker is gone. The value the gates read is the core's own
per-round `finalText` (the same computation that becomes `Result.FinalText`),
handed to the policy immediately before `CanFinish` via the optional
`RoundFinalTextReceiver` interface — provably tied to the round boundary,
`""` for a textless round by construction. The scheduled policy implements the
interface (compile-time asserted) and both Gate 1 (verifier) and Gate 2
(phone-a-friend — which had the same blind spot, reviewing an empty answer on
every scheduled run) read it through `latestRunText()`.

## Tests

- `TestScheduledReviewerRepairExhaustsVerificationCap` — reject, reject,
  accept (cap spent), reviewer forces a repair → NO fourth verifier call; the
  run ends as `ErrCompletionUnverified` naming the unverifiable repair.
- `TestPolicyRoundTextPanic_IsContainedAsRunError` (agentcore) — a panicking
  `SetRoundFinalText` is contained exactly like a panicking `CanFinish`:
  `ErrRunBoundaryPanic`, one policy-finish panic event, partial transcript and
  usage preserved.
- `TestScheduledReviewerRepairReverifies` — Gate 2 forces a repair; the
  repaired round's closing text goes through the verifier again (the second
  prompt carries the revised answer, not the pre-repair one). Pre-fix the
  verifier ran once and the repaired answer shipped unverified.
- `TestScheduledVerifierTextlessRepairRoundSeesNoResponseMarker` — the P1
  reproduction end-to-end: prose round → verifier rejects for a missing action
  → repair round makes only the tool call → the re-check's prompt carries the
  `(no final response text)` marker AND the fresh tool evidence, and NOT the
  rejected round's report.
- `TestRoundFinalTextReceiver_PinnedToRoundBoundary` (agentcore) — the seam
  hands over the streaming round's text, then exactly `""` for a textless
  repair round.
- `TestRunEndOfRunVerifierIncludesFinalResponse` — the prompt the verifier
  model receives contains the FINAL RESPONSE section with the latest assistant
  text.
- `TestTruncateFinalResponseForVerifier` — bound, head+tail kept, cut marked;
  empty → explicit marker.
- `TestRunEndOfRunVerifierEmptyFinalResponseMarked` — empty text reaches the
  verifier as `(no final response text)`.
- `TestScheduledCompletionAfterCommittedWrite` (extended) — the scheduled
  driver end-to-end: Gate 1's verifier prompt carries the run's closing message
  across the real core seam, in both the clean and the
  exhausted-verification paths.

## Deviations

- The brief said "pass `latestAssistantText` into `runEndOfRunVerifier`";
  shipped as the core's per-round `finalText` through `RoundFinalTextReceiver`,
  because the session-only reading is a no-op at the gate seam (the session
  gains the closing message only after `Run` returns) and an observer-side
  tracker is not provably tied to the round boundary (Codex P1, fixed in
  commit 2).
- Codex P2 (round 2): running the finish gates against the terminal
  structured-output JSON was evaluated and rejected as too invasive — it would
  move a paid terminal generation inside the finish gate and leave repair
  semantics undefined. Chosen instead: the behavior is documented honestly
  (contract comment, this note, AGENT-RUNTIME.md) — structured-output tasks
  are verified on their free-form closing text, and the JSON is
  schema-validated by the terminal phase.
- Gate 2's answer source changed with Gate 1's (same seam). It is the behavior
  the reviewer was always documented to have ("the answer/work the reviewer
  critiques") and the feature is off unless an operator enables phone-a-friend.

## Deferred

- No change to what makes a run fail: the verifier's demands for tool-backed
  deliverables are untouched; this only adds evidence it was blind to.
- Interactive (chat) runs have no end-of-run verifier — unchanged.
- The persisted captain's log shape is unchanged (only the completed response
  lands in the session, as before).
