import { describe, expect, it } from "vitest";
import type { Message } from "../history";
import { buildReplyMessage, buildSubmissionMessage } from "./model";
import { deriveGenUiState, isRenderableCardCall, summarizeValue } from "./transcript";

const card = (title: string, extra: Record<string, unknown> = {}) =>
  JSON.stringify({ title, components: [{ type: "text", text: "x" }], ...extra });

function assistant(id: number, calls: { id: string; input: string; resultText?: string; state?: "done" | "error" | "pending" }[]): Message {
  return {
    id,
    role: "assistant",
    content: "",
    state: "done",
    toolCalls: calls.map((c) => ({ name: "show_ui", state: c.state ?? "done", ...c })),
  } as unknown as Message;
}

function user(id: number, content: string): Message {
  return { id, role: "user", content, state: "done" } as unknown as Message;
}

describe("deriveGenUiState", () => {
  it("collects cards, the latest submission per card, and replaced cards", () => {
    const msgs = [
      assistant(1, [{ id: "c1", input: card("One"), resultText: "UI_DISPLAYED card_id=c1" }]),
      user(2, buildSubmissionMessage("c1", "check", { a: 1 })),
      assistant(3, [{ id: "c2", input: card("Two", { replaces: "c1" }), resultText: "UI_DISPLAYED card_id=c2" }]),
      user(4, buildSubmissionMessage("c2", "check", { a: 2 })),
      user(5, buildSubmissionMessage("c2", "create", { a: 3 })),
    ];
    const s = deriveGenUiState(msgs);
    expect([...s.cards.keys()]).toEqual(["c1", "c2"]);
    expect(s.superseded.has("c1")).toBe(true);
    expect(s.submissions.get("c2")?.actionId).toBe("create");
    expect(s.submissions.get("c1")?.values).toEqual({ a: 1 });
  });

  it("does not draw a refused or still-pending spec", () => {
    const refused = { id: "x", name: "show_ui", input: card("Bad"), resultText: "UI_INVALID: …", state: "done" as const };
    expect(isRenderableCardCall(refused)).toBe(false);
    expect(isRenderableCardCall({ ...refused, state: "error" })).toBe(false);
    expect(isRenderableCardCall({ ...refused, resultText: undefined, state: "pending" })).toBe(false);
    // A call with no stored result (cancelled turn) never passed validation.
    expect(isRenderableCardCall({ ...refused, resultText: undefined, state: "done" })).toBe(false);
    expect(isRenderableCardCall({ ...refused, resultText: "UI_DISPLAYED card_id=x" })).toBe(true);
    expect(isRenderableCardCall({ ...refused, name: "bash", resultText: "UI_DISPLAYED" })).toBe(false);
  });
});

describe("summarizeValue", () => {
  it("renders each value shape in one line", () => {
    expect(summarizeValue("")).toBe("—");
    expect(summarizeValue(true)).toBe("Yes");
    expect(summarizeValue(["a", "b"])).toBe("a, b");
    expect(summarizeValue([{ a: 1 }, { a: 2 }])).toBe("2 items");
    expect(summarizeValue({ include: ["a"], exclude: [] })).toBe("1 included · 0 excluded");
    expect(summarizeValue(Array.from({ length: 10 }, (_, i) => `d${i}`))).toBe("d0, d1, d2, d3, d4, d5, d6, d7 +2 more");
  });
});

describe("quick replies", () => {
  it("attribute a reply by its marker, never by matching text", () => {
    const quick = (title: string) =>
      JSON.stringify({
        title,
        components: [{ type: "text", text: "?" }],
        actions: [{ id: "yes", label: "Yes", kind: "message", message: "Yes, go ahead" }],
      });
    const s = deriveGenUiState([
      assistant(1, [{ id: "c1", input: quick("One"), resultText: "UI_DISPLAYED card_id=c1" }]),
      assistant(2, [{ id: "c2", input: quick("Two"), resultText: "UI_DISPLAYED card_id=c2" }]),
      // Typed by hand: same words, no marker — locks nothing.
      user(3, "Yes, go ahead"),
      user(4, buildReplyMessage("c1", "yes", "Yes, go ahead")),
    ]);
    expect(s.replies.get("c1")).toEqual({ cardId: "c1", actionId: "yes", text: "Yes, go ahead", messageId: 4 });
    expect(s.replies.has("c2")).toBe(false);
  });
});

describe("refused card messages", () => {
  it("do not lock the card when their turn failed before doing anything", () => {
    const failed = { id: 3, role: "assistant", content: "Turn failed", state: "done", failed: true } as unknown as Message;
    const s = deriveGenUiState([
      assistant(1, [{ id: "c1", input: card("One"), resultText: "UI_DISPLAYED card_id=c1" }]),
      user(2, buildSubmissionMessage("c1", "go", { a: 1 })),
      failed,
    ]);
    expect(s.submissions.has("c1")).toBe(false);
  });
});
