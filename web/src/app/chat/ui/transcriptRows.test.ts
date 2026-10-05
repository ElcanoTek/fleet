import { describe, expect, it } from "vitest";
import {
  buildTranscriptRows,
  messageHasRenderableContent,
  showsEmptyReplyNotice,
  type BuildTranscriptRowsInput,
} from "./transcriptRows";
import type { Message } from "./history";

const userMsg = (id: number, content: string, injectedContext?: string): Message => ({
  id,
  role: "user",
  content,
  injectedContext,
  state: "done",
});
const assistantMsg = (id: number, content: string, state: Message["state"] = "done"): Message => ({
  id,
  role: "assistant",
  content,
  state,
});

const build = (
  messages: Message[],
  over: Partial<BuildTranscriptRowsInput> = {},
) =>
  buildTranscriptRows({
    messages,
    summaryIndex: -1,
    summaryExpanded: false,
    isSummarizing: false,
    summarizeError: null,
    ...over,
  });

describe("messageHasRenderableContent", () => {
  it("keeps a user turn with text", () => {
    expect(messageHasRenderableContent(userMsg(1, "hello"))).toBe(true);
  });

  it("keeps a user turn whose only content is injected context", () => {
    expect(messageHasRenderableContent(userMsg(1, "", "\n---\n**User attached files:**"))).toBe(
      true,
    );
  });

  it("drops a user turn with nothing at all", () => {
    expect(messageHasRenderableContent(userMsg(1, ""))).toBe(false);
    expect(messageHasRenderableContent(userMsg(1, "   \n "))).toBe(false);
    expect(messageHasRenderableContent(userMsg(1, "", "  "))).toBe(false);
  });

  it("never drops an assistant turn — empty ones still draw an affordance", () => {
    // In flight: the thinking indicator. Finished with no text: the
    // "finished without a written reply" safety net.
    expect(messageHasRenderableContent(assistantMsg(1, "", "thinking"))).toBe(true);
    expect(messageHasRenderableContent(assistantMsg(1, ""))).toBe(true);
  });

  it("never drops a compaction summary — the banner is its own chrome", () => {
    expect(
      messageHasRenderableContent({ ...assistantMsg(1, ""), kind: "summary" }),
    ).toBe(true);
  });
});

describe("buildTranscriptRows", () => {
  it("emits one message row per message, in order", () => {
    const rows = build([userMsg(1, "q"), assistantMsg(2, "a")]);
    expect(rows).toEqual([
      { kind: "message", message: userMsg(1, "q"), isPreSummary: false },
      { kind: "message", message: assistantMsg(2, "a"), isPreSummary: false },
    ]);
  });

  // QA finding #11: an image-only user turn in a teammate-branched chat came
  // across with its images stripped. Branches created before the server-side
  // fix still hold that row, so the renderer must emit NO element for it —
  // an empty bubble is a container whose height is decided by padding and
  // line-box rounding, which is exactly what the stick-to-bottom loop
  // oscillates against.
  it("emits no row for residue left by a transcript-only branch copy", () => {
    const rows = build([userMsg(1, "q"), assistantMsg(2, "a"), userMsg(3, "")]);
    expect(rows).toHaveLength(2);
    expect(rows.some((r) => r.kind === "message" && r.message.id === 3)).toBe(false);
  });

  it("keeps the leading summarize card and error rows", () => {
    const rows = build([userMsg(1, "q")], {
      isSummarizing: true,
      summarizeError: "nope",
    });
    expect(rows[0]).toEqual({ kind: "summarizing" });
    expect(rows[1]).toEqual({ kind: "summarize-error" });
  });

  it("collapses the pre-summary range behind a single expander", () => {
    const messages = [
      userMsg(1, "old q"),
      assistantMsg(2, "old a"),
      { ...assistantMsg(3, "summary text"), kind: "summary" as const },
      userMsg(4, "new q"),
    ];
    const rows = build(messages, { summaryIndex: 2, summaryExpanded: false });
    expect(rows.map((r) => r.kind)).toEqual(["expander", "summary", "message"]);
  });

  it("shows the pre-summary range dimmed when expanded", () => {
    const messages = [
      userMsg(1, "old q"),
      { ...assistantMsg(2, "summary text"), kind: "summary" as const },
      userMsg(3, "new q"),
    ];
    const rows = build(messages, { summaryIndex: 1, summaryExpanded: true });
    expect(rows.map((r) => r.kind)).toEqual(["message", "summary", "message"]);
    expect(rows[0]).toMatchObject({ isPreSummary: true });
    expect(rows[2]).toMatchObject({ isPreSummary: false });
  });

  it("still emits the expander when the first collapsed turn is itself empty", () => {
    // The expander stands in for the whole hidden RANGE, not for message 0.
    const messages = [
      userMsg(1, ""),
      { ...assistantMsg(2, "summary text"), kind: "summary" as const },
    ];
    const rows = build(messages, { summaryIndex: 1, summaryExpanded: false });
    expect(rows.map((r) => r.kind)).toEqual(["expander", "summary"]);
  });
});

// The empty-reply safety net says the turn COMPLETED without an answer. Every
// other terminal state with its own banner has to silence it, or the transcript
// tells two contradictory stories about one turn — which is what a stopped turn
// did after a reload, when replay forgot to mark it cancelled.
describe("showsEmptyReplyNotice", () => {
  it("shows for a finished assistant turn with no written reply", () => {
    expect(showsEmptyReplyNotice(assistantMsg(1, ""))).toBe(true);
    expect(showsEmptyReplyNotice(assistantMsg(1, "  \n "))).toBe(true);
  });

  it("stays quiet when there is an answer, or the turn is still running", () => {
    expect(showsEmptyReplyNotice(assistantMsg(1, "the answer"))).toBe(false);
    expect(showsEmptyReplyNotice(assistantMsg(1, "", "thinking"))).toBe(false);
    expect(showsEmptyReplyNotice(assistantMsg(1, "", "streaming"))).toBe(false);
  });

  it("defers to a stopped turn's own banner", () => {
    expect(showsEmptyReplyNotice({ ...assistantMsg(1, ""), cancelled: true })).toBe(false);
  });

  it("defers to the failed, model-required and retrying banners", () => {
    expect(showsEmptyReplyNotice({ ...assistantMsg(1, ""), failed: true })).toBe(false);
    expect(
      showsEmptyReplyNotice({
        ...assistantMsg(1, ""),
        modelRequired: { reason: "fatal", failedModel: "m", statusCode: 0, message: "x" },
      }),
    ).toBe(false);
    expect(
      showsEmptyReplyNotice({
        ...assistantMsg(1, ""),
        retrying: { statusCode: 429, title: "", message: "", delayMs: 0 },
      }),
    ).toBe(false);
  });

  it("defers to approval and memory cards, which own the turn", () => {
    expect(
      showsEmptyReplyNotice({
        ...assistantMsg(1, ""),
        approvals: [{ id: "a1", tool: "bash", summary: {}, status: "pending" }],
      }),
    ).toBe(false);
    expect(
      showsEmptyReplyNotice({
        ...assistantMsg(1, ""),
        memoryProposals: [{ id: "p1", content: "fact", status: "pending" }],
      }),
    ).toBe(false);
  });
});
