import { describe, expect, it } from "vitest";
import type { Message } from "../history";
import { buildReplyMessage, buildSubmissionMessage } from "./model";
import { cardSpecOf, deriveGenUiState, fieldLabels, isRenderableCardCall, passwordFields, summarizeValue } from "./transcript";

const card = (title: string, extra: Record<string, unknown> = {}) =>
  JSON.stringify({ title, components: [{ type: "text", text: "x" }], ...extra });

function assistant(
  id: number,
  calls: { id: string; input: string; resultText?: string; card?: string; state?: "done" | "error" | "pending" }[],
): Message {
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

describe("cards shown by reference (from_result)", () => {
  const ref = JSON.stringify({ from_result: { tool: "intake_card" }, replaces: "c0" });

  it("draws the validated spec the result carries, never the reference input", () => {
    const msgs = [
      assistant(1, [{ id: "c1", input: ref, card: card("Intake", { replaces: "c0" }), resultText: "UI_DISPLAYED card_id=c1" }]),
    ];
    const tc = msgs[0].toolCalls![0];
    expect(isRenderableCardCall(tc)).toBe(true);
    expect(cardSpecOf(tc)?.title).toBe("Intake");
    const s = deriveGenUiState(msgs);
    expect(s.cards.get("c1")?.title).toBe("Intake");
    expect(s.superseded.has("c0")).toBe(true);
  });

  it("draws nothing for a reference whose result carries no card", () => {
    const tc = assistant(1, [{ id: "c1", input: ref, resultText: "UI_DISPLAYED card_id=c1" }]).toolCalls![0];
    expect(isRenderableCardCall(tc)).toBe(false);
    expect(deriveGenUiState([assistant(1, [{ id: "c1", input: ref, resultText: "UI_DISPLAYED card_id=c1" }])]).cards.size).toBe(0);
  });

  it("leaves an inline card that merely mentions from_result alone", () => {
    const inline = JSON.stringify({ title: "T", components: [{ type: "text", text: '"from_result"' }] });
    const tc = assistant(1, [{ id: "c1", input: inline, resultText: "UI_DISPLAYED card_id=c1" }]).toolCalls![0];
    expect(isRenderableCardCall(tc)).toBe(true);
    expect(cardSpecOf(tc)?.title).toBe("T");
  });
});

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

  it("gives a reused tool-call id to the newest card; the older keeps its own spec", () => {
    const msgs = [
      assistant(1, [{ id: "call_0", input: card("First"), resultText: "UI_DISPLAYED card_id=call_0" }]),
      assistant(2, [{ id: "call_0", input: card("Second"), resultText: "UI_DISPLAYED card_id=call_0" }]),
    ];
    const s = deriveGenUiState(msgs);
    const [older, newer] = [msgs[0].toolCalls![0], msgs[1].toolCalls![0]];
    expect(s.owners.get("call_0")).toBe(newer);
    expect(s.cards.get("call_0")?.title).toBe("Second");
    expect(s.owners.get("call_0")).not.toBe(older);
    expect(cardSpecOf(older)?.title).toBe("First");
    // Each occurrence keeps its browser state under its own key.
    expect(s.occurrences.get(older)).toBe(0);
    expect(s.occurrences.get(newer)).toBe(1);
  });

  it("keeps an earlier card's answer and replacement off a newer card that reuses its id", () => {
    const msgs = [
      assistant(1, [{ id: "call_0", input: card("First"), resultText: "UI_DISPLAYED card_id=call_0" }]),
      user(2, buildSubmissionMessage("call_0", "go", { a: 1 })),
      assistant(3, [{ id: "call_0", input: card("Second", { replaces: "call_0" }), resultText: "UI_DISPLAYED card_id=call_0" }]),
    ];
    const s = deriveGenUiState(msgs);
    expect(s.cards.get("call_0")?.title).toBe("Second");
    expect(s.submissions.has("call_0")).toBe(false);
    expect(s.superseded.has("call_0")).toBe(false);
    // An answer after the newer card is its own.
    const later = deriveGenUiState([...msgs, user(4, buildSubmissionMessage("call_0", "go", { a: 2 }))]);
    expect(later.submissions.get("call_0")?.values).toEqual({ a: 2 });
  });

  it("labels each answer with the card it answered, even when a later card reuses the id", () => {
    const msgs = [
      assistant(1, [{ id: "call_0", input: card("First"), resultText: "UI_DISPLAYED card_id=call_0" }]),
      user(2, buildSubmissionMessage("call_0", "go", { a: 1 })),
      assistant(3, [{ id: "call_0", input: card("Second"), resultText: "UI_DISPLAYED card_id=call_0" }]),
      user(4, buildSubmissionMessage("call_0", "go", { a: 2 })),
    ];
    const s = deriveGenUiState(msgs);
    expect(s.answerSpecs.get(msgs[1])?.title).toBe("First");
    expect(s.answerSpecs.get(msgs[3])?.title).toBe("Second");
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
  const failed = { id: 3, role: "assistant", content: "Turn failed", state: "done", failed: true } as unknown as Message;

  it("do not lock the card when the server never took them", () => {
    const s = deriveGenUiState([
      assistant(1, [{ id: "c1", input: card("One"), resultText: "UI_DISPLAYED card_id=c1" }]),
      { ...user(2, buildSubmissionMessage("c1", "go", { a: 1 })), notSent: true },
      failed,
    ]);
    expect(s.submissions.has("c1")).toBe(false);
  });

  it("do not lock the card when stopped before the server took them", () => {
    const cancelled = { id: 3, role: "assistant", content: "", state: "done", cancelled: true } as unknown as Message;
    const s = deriveGenUiState([
      assistant(1, [{ id: "c1", input: card("One"), resultText: "UI_DISPLAYED card_id=c1" }]),
      { ...user(2, buildSubmissionMessage("c1", "go", { a: 1 })), notSent: true },
      cancelled,
    ]);
    expect(s.submissions.has("c1")).toBe(false);
  });

  it("still lock the card when they were accepted and the turn then failed", () => {
    const modelRequired = {
      id: 3,
      role: "assistant",
      content: "",
      state: "done",
      failed: true,
      modelRequired: { message: "pick another", failedModel: "x" },
    } as unknown as Message;
    for (const next of [failed, modelRequired]) {
      const s = deriveGenUiState([
        assistant(1, [{ id: "c1", input: card("One"), resultText: "UI_DISPLAYED card_id=c1" }]),
        user(2, buildSubmissionMessage("c1", "go", { a: 1 })),
        next,
      ]);
      expect(s.submissions.get("c1")?.actionId).toBe("go");
    }
  });
});

describe("one answer per card", () => {
  it("is the latest, across submissions and quick replies", () => {
    const s = deriveGenUiState([
      assistant(1, [{ id: "c1", input: card("One"), resultText: "UI_DISPLAYED card_id=c1" }]),
      user(2, buildSubmissionMessage("c1", "go", { a: 1 })),
      user(3, buildReplyMessage("c1", "cancel", "Never mind")),
    ]);
    expect(s.submissions.has("c1")).toBe(false);
    expect(s.replies.get("c1")?.actionId).toBe("cancel");
    const t = deriveGenUiState([
      assistant(1, [{ id: "c1", input: card("One"), resultText: "UI_DISPLAYED card_id=c1" }]),
      user(2, buildReplyMessage("c1", "cancel", "Never mind")),
      user(3, buildSubmissionMessage("c1", "go", { a: 2 })),
    ]);
    expect(t.replies.has("c1")).toBe(false);
    expect(t.submissions.get("c1")?.values).toEqual({ a: 2 });
  });
});

describe("an unconfirmed card answer", () => {
  it("does not lock the card while recovery is still settling it", () => {
    const streaming = { id: 3, role: "assistant", content: "", state: "streaming" } as unknown as Message;
    const s = deriveGenUiState([
      assistant(1, [{ id: "c1", input: card("One"), resultText: "UI_DISPLAYED card_id=c1" }]),
      { ...user(2, buildSubmissionMessage("c1", "go", { a: 1 })), notSent: true },
      streaming,
    ]);
    expect(s.submissions.has("c1")).toBe(false);
  });

  it("locks the card once a finished reply shows the server took it", () => {
    const done = { id: 3, role: "assistant", content: "Thanks, creating them now.", state: "done" } as unknown as Message;
    const s = deriveGenUiState([
      assistant(1, [{ id: "c1", input: card("One"), resultText: "UI_DISPLAYED card_id=c1" }]),
      { ...user(2, buildSubmissionMessage("c1", "go", { a: 1 })), notSent: true },
      done,
    ]);
    expect(s.submissions.get("c1")?.actionId).toBe("go");
  });
});

describe("summarizeValue", () => {
  it("bounds long values in the compact bubble", () => {
    const long = "x".repeat(5000);
    expect(summarizeValue(long).length).toBeLessThanOrEqual(121);
    expect(summarizeValue([long]).length).toBeLessThanOrEqual(121);
    expect(summarizeValue("short")).toBe("short");
  });
});

describe("fieldLabels", () => {
  it("falls back to the id for a blank label, and trims the rest", () => {
    const labels = fieldLabels({
      title: "x",
      components: [
        { type: "text_input", id: "a", label: " " },
        { type: "text_input", id: "b", label: "  Name " },
        { type: "repeater", id: "r", label: "R", fields: [{ type: "text_input", id: "f", label: "\t" }] },
      ],
    } as never);
    expect(labels.get("a")).toBe("a");
    expect(labels.get("b")).toBe("Name");
    expect(labels.get("f")).toBe("f");
  });
});

describe("passwordFields", () => {
  it("collects format=password text_input ids including repeater fields", () => {
    const secrets = passwordFields({
      title: "Login",
      components: [
        { type: "text_input", id: "username", label: "Username" },
        { type: "text_input", id: "password", label: "Password", format: "password" },
        {
          type: "repeater",
          id: "tokens",
          label: "Tokens",
          fields: [{ type: "text_input", id: "secret_token", label: "Token", format: "password" }],
        },
      ],
    } as never);
    expect([...secrets]).toEqual(["password", "secret_token"]);
  });
});

describe("derivation cost", () => {
  it("parses each answer and card once, not on every streamed update", () => {
    const sub = user(2, buildSubmissionMessage("c1", "go", { n: "x".repeat(1000) }));
    const shown = assistant(1, [{ id: "c1", input: card("Big"), resultText: "UI_DISPLAYED" }]);
    const first = deriveGenUiState([shown, sub]);
    // A streamed delta replaces the array (and the streaming message), not
    // the finished ones.
    const second = deriveGenUiState([shown, sub, assistant(3, [])]);
    expect(second.submissions.get("c1")?.values).toBe(first.submissions.get("c1")?.values);
    expect(second.cards.get("c1")).toBe(first.cards.get("c1"));
    // A message whose text changed is parsed afresh.
    const edited = { ...sub, content: buildSubmissionMessage("c1", "go", { n: "y" }) } as Message;
    expect(deriveGenUiState([shown, edited]).submissions.get("c1")?.values).toEqual({ n: "y" });
  });
});

describe("dashboard panel keys", () => {
  it("a later card with the same panel key replaces the earlier one", () => {
    const s = deriveGenUiState([
      assistant(1, [{ id: "a", input: card("Revenue", { panel: "revenue" }), resultText: "UI_DISPLAYED card_id=a" }]),
      assistant(2, [{ id: "b", input: card("Deals", { panel: "deals" }), resultText: "UI_DISPLAYED card_id=b" }]),
      assistant(3, [{ id: "c", input: card("Revenue", { panel: "revenue" }), resultText: "UI_DISPLAYED card_id=c" }]),
    ]);
    expect([...s.superseded]).toEqual(["a"]);
    expect(s.cards.get("c")?.panel).toBe("revenue");
  });

  it("drops a panel key the server would refuse", () => {
    const s = deriveGenUiState([
      assistant(1, [{ id: "a", input: card("R", { panel: "Not A Key" }), resultText: "UI_DISPLAYED card_id=a" }]),
    ]);
    expect(s.cards.get("a")?.panel).toBeUndefined();
  });
});

describe("panel ownership through replacements", () => {
  it("a keyless refinement takes its panel over, so the next same-key card supersedes it", () => {
    const s = deriveGenUiState([
      assistant(1, [{ id: "a", input: card("Rev", { panel: "rev" }), resultText: "UI_DISPLAYED card_id=a" }]),
      assistant(2, [{ id: "b", input: card("Rev fixed", { replaces: "a" }), resultText: "UI_DISPLAYED card_id=b" }]),
      assistant(3, [{ id: "c", input: card("Rev v3", { panel: "rev" }), resultText: "UI_DISPLAYED card_id=c" }]),
    ]);
    expect(s.superseded.has("a")).toBe(true);
    expect(s.superseded.has("b")).toBe(true);
    expect(s.superseded.has("c")).toBe(false);
  });

  it("a refinement of a card that no longer holds the panel does not take it", () => {
    const s = deriveGenUiState([
      assistant(1, [{ id: "a", input: card("Rev", { panel: "rev" }), resultText: "UI_DISPLAYED card_id=a" }]),
      assistant(2, [{ id: "c", input: card("Rev v2", { panel: "rev" }), resultText: "UI_DISPLAYED card_id=c" }]),
      assistant(3, [{ id: "b", input: card("Old fixed", { replaces: "a" }), resultText: "UI_DISPLAYED card_id=b" }]),
    ]);
    expect(s.superseded.has("c")).toBe(false);
  });
});

describe("panel ownership and reused ids", () => {
  it("an unrelated card reusing a panel card's id is not superseded by that panel's refresh", () => {
    const s = deriveGenUiState([
      assistant(1, [{ id: "a", input: card("Rev", { panel: "x" }), resultText: "UI_DISPLAYED card_id=a" }]),
      assistant(2, [{ id: "a", input: card("Unrelated"), resultText: "UI_DISPLAYED card_id=a" }]),
      assistant(3, [{ id: "b", input: card("Rev v2", { panel: "x" }), resultText: "UI_DISPLAYED card_id=b" }]),
    ]);
    expect(s.superseded.has("a")).toBe(false);
    expect(s.cards.get("a")?.title).toBe("Unrelated");
  });
});

describe("panel moves retire the old owner", () => {
  it("a stale card's later refinement does not inherit the panel its lineage left", () => {
    const s = deriveGenUiState([
      assistant(1, [{ id: "a", input: card("A", { panel: "x" }), resultText: "UI_DISPLAYED card_id=a" }]),
      assistant(2, [{ id: "c", input: card("C", { panel: "y", replaces: "a" }), resultText: "UI_DISPLAYED card_id=c" }]),
      assistant(3, [{ id: "b", input: card("B", { replaces: "a" }), resultText: "UI_DISPLAYED card_id=b" }]),
      assistant(4, [{ id: "n", input: card("New X", { panel: "x" }), resultText: "UI_DISPLAYED card_id=n" }]),
    ]);
    expect(s.superseded.has("b")).toBe(false);
  });
});
