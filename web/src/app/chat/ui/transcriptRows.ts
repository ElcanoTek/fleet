// The transcript's row model — the flat, stable-indexed list the virtualizer
// consumes, plus the one rule that decides whether a message is worth a row at
// all.
//
// Extracted from ChatTranscript's inline useMemo so both halves are unit
// testable without jsdom (which does no layout, so a virtualized transcript
// renders nothing there).

import type { Message } from "./history";

export type TranscriptRow =
  // Live compaction progress card (only while summarizing).
  | { kind: "summarizing" }
  // Compaction error banner.
  | { kind: "summarize-error" }
  // The single "Show N earlier turns" expander that stands in for the whole
  // collapsed pre-summary range.
  | { kind: "expander" }
  // A compaction summary banner message.
  | { kind: "summary"; message: Message }
  // A normal user/assistant turn. `isPreSummary` dims pre-compaction turns when
  // the range is expanded (matching the former inline opacity-60 rule).
  | { kind: "message"; message: Message; isPreSummary: boolean };

/**
 * messageHasRenderableContent answers: would this message draw anything?
 *
 * Transcript-only teammate copies omit image attachments. Older copies may
 * contain a user text row emptied by that omission; rendering it would draw
 * a blank bubble and allocate an unnecessary virtual row. Skip that residue.
 * This is content cleanup, not the fix for image/measurement oscillation:
 * AssistantContent must preserve component identities for that.
 *
 * Deliberately conservative about what it drops:
 *
 *   - A user turn renders nothing when it has neither text nor injected
 *     context. Nothing else hangs off a user row: the Edit affordance only
 *     appears on the last user turn, and an empty message cannot be resent
 *     anyway (UserBubble's save requires a non-blank draft).
 *   - An assistant turn ALWAYS renders something, so it is never dropped: in
 *     flight it draws the thinking indicator, and a finished turn with no text
 *     draws the "finished without a written reply" safety net. Dropping a live
 *     assistant slot would delete the only sign that a turn is running.
 *   - A compaction summary row always renders its banner chrome.
 */
export function messageHasRenderableContent(message: Message): boolean {
  if (message.kind === "summary") return true;
  if (message.role !== "user") return true;
  return Boolean(message.content.trim() || message.injectedContext?.trim());
}

export type BuildTranscriptRowsInput = {
  messages: Message[];
  /** Index into `messages` of the compaction summary, or -1 when there is none. */
  summaryIndex: number;
  summaryExpanded: boolean;
  isSummarizing: boolean;
  summarizeError: string | null;
};

/**
 * buildTranscriptRows flattens the message list (plus the leading summarize
 * card/error and the compaction expander) into the explicit, stable-indexed
 * row model the virtualizer consumes. It reproduces the pre-virtualization
 * map's control flow: when a compaction summary exists and the pre-summary
 * range is collapsed, the whole range [0, summaryIndex) is represented by a
 * single expander row.
 */
export function buildTranscriptRows({
  messages,
  summaryIndex,
  summaryExpanded,
  isSummarizing,
  summarizeError,
}: BuildTranscriptRowsInput): TranscriptRow[] {
  const out: TranscriptRow[] = [];
  if (isSummarizing) out.push({ kind: "summarizing" });
  if (summarizeError) out.push({ kind: "summarize-error" });
  const collapsed = summaryIndex >= 0 && !summaryExpanded;
  messages.forEach((message, idx) => {
    const isPreSummary = summaryIndex >= 0 && idx < summaryIndex;
    if (isPreSummary && collapsed) {
      // The single expander stands in for the first hidden turn; every other
      // hidden turn is skipped (matches the former `return null`). It stands
      // for the RANGE, so it is emitted regardless of whether message 0 itself
      // has anything to draw.
      if (idx === 0) out.push({ kind: "expander" });
      return;
    }
    if (message.kind === "summary") {
      out.push({ kind: "summary", message });
      return;
    }
    // Residue from a transcript-only copy: no row rather than an empty one.
    if (!messageHasRenderableContent(message)) return;
    out.push({ kind: "message", message, isPreSummary });
  });
  return out;
}
