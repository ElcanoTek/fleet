import type { Message } from "./history";
import { joinTextSegments } from "./textSegments";

// Working narration vs. the answer, for a live assistant turn.
//
// A tool-using turn is several model steps. Before each tool call the model
// usually writes a line of narration ("Let me pull the deal first."), and the
// server streams every step's text as text.delta into one string; only at the
// end does text.replace swap in the persisted final answer. Rendered as-is,
// the default chat view showed that running narration — steps glued together
// ("deal.Found it."), internals on display, and the live "working" status
// gone from the moment the first sentence arrived, so a busy agent looked
// stalled. The helpers below keep the two apart:
//
//   - A tool.call ends a step, so whatever text that step streamed is
//     narration: it moves from `content` to `narration` (foldNarration) and
//     `content` holds only the step now streaming.
//   - The default view shows the tidy status line, not the narration, and
//     holds back a short in-progress segment that may yet turn out to be more
//     narration (assistantDisplayContent). "Show details" shows everything.
//   - A turn that ends WITHOUT text.replace (stopped, failed, round cap) keeps
//     its partial transcript, as before: the narration is folded back into the
//     content (settleNarration).
//
// The server separates steps itself (agentcore.StepTextSeparator), so these
// joins are also the fallback for a stream from an older server.

/**
 * A segment of narration shorter than this is held back in the default view
 * while the turn is still streaming. Narration is a sentence or two (well
 * under this); an answer outgrows it within the first second of streaming,
 * and one that never does appears when the turn ends.
 */
export const NARRATION_HOLD_CHARS = 280;

/**
 * foldNarration runs on tool.call: the text streamed so far in this step was
 * narration ahead of the call, not the answer. A message with no streamed
 * text, or whose answer is already final, is returned unchanged.
 */
export function foldNarration(m: Message): Message {
  if (m.finalAnswer || !m.content.trim()) return m;
  return { ...m, narration: joinTextSegments(m.narration ?? "", m.content), content: "" };
}

/**
 * settleNarration runs on every terminal turn event. With text.replace seen,
 * `content` is the authoritative answer and the narration stays for "Show
 * details". Without it the turn stopped short, and what it streamed is the
 * whole record, so the narration goes back in front of the content.
 */
export function settleNarration(m: Message): Message {
  if (m.finalAnswer || !m.narration) return m;
  return { ...m, content: joinTextSegments(m.narration, m.content), narration: undefined };
}

/**
 * assistantDisplayContent is the text the transcript renders as the answer.
 * Settled turns and the detailed view render `content` as is. While a turn
 * streams in the default view, a short segment that is not yet known to be
 * the answer is held back (""), so the status line stays up instead of
 * narration flashing in and out between tool calls.
 */
export function assistantDisplayContent(m: Message, showDetails: boolean): string {
  if (showDetails || m.finalAnswer || m.state !== "streaming") return m.content;
  return m.content.trim().length < NARRATION_HOLD_CHARS ? "" : m.content;
}
