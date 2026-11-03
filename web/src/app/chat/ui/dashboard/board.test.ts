import { describe, expect, it } from "vitest";
import type { Message } from "../history";
import { deriveGenUiState } from "../genui/transcript";
import { boardStatus, deriveBoard, dropKey, EMPTY_BOARD_PREFS, moveKey, prefersWide, type BoardPrefs } from "./board";

const display = (title: string, extra: Record<string, unknown> = {}) =>
  JSON.stringify({ title, components: [{ type: "stat", label: "x", value: "1" }], ...extra });
const form = (title: string, extra: Record<string, unknown> = {}) =>
  JSON.stringify({
    title,
    components: [{ type: "text_input", id: "n", label: "N" }],
    actions: [{ id: "go", label: "Go" }],
    ...extra,
  });

function assistant(id: number, calls: { id: string; input: string; state?: "done" | "pending" }[]): Message {
  return {
    id,
    role: "assistant",
    content: "",
    state: "done",
    toolCalls: calls.map((c) => ({ name: "show_ui", state: c.state ?? "done", resultText: `UI_DISPLAYED card_id=${c.id}`, ...c })),
  } as unknown as Message;
}

function boardOf(messages: Message[], prefs: BoardPrefs = EMPTY_BOARD_PREFS) {
  return deriveBoard(messages, deriveGenUiState(messages), prefs);
}

describe("deriveBoard", () => {
  const msgs = [
    assistant(1, [
      { id: "rev1", input: display("Revenue", { panel: "revenue" }) },
      { id: "sys", input: display("Systems") },
      { id: "ask", input: form("Follow up") },
    ]),
    assistant(2, [{ id: "rev2", input: display("Revenue v2", { panel: "revenue" }) }]),
  ];

  it("puts panel cards and display-only cards on the board, forms in the chat", () => {
    const b = boardOf(msgs);
    expect(b.panels.map((p) => p.key)).toEqual(["p:revenue", "c:sys"]);
    expect(b.cardSlots.has("ask")).toBe(false);
  });

  it("a panel key keeps one slot and draws its newest card", () => {
    const rev = boardOf(msgs).panels[0];
    expect(rev.cardId).toBe("rev2");
    expect(rev.spec.title).toBe("Revenue v2");
    expect(rev.version).toBe(2);
    expect(rev.messageIndex).toBe(1);
    // Both versions point at the slot, so the rail draws neither twice.
    expect(boardOf(msgs).cardSlots.get("rev1")).toBe("p:revenue");
    expect(boardOf(msgs).cardSlots.get("rev2")).toBe("p:revenue");
  });

  it("a card that replaces a panel card takes over its slot", () => {
    const b = boardOf([
      assistant(1, [{ id: "a", input: display("Spend") }]),
      assistant(2, [{ id: "b", input: display("Spend (fixed)", { replaces: "a" }) }]),
    ]);
    expect(b.panels).toHaveLength(1);
    expect(b.panels[0]).toMatchObject({ key: "c:a", cardId: "b", version: 2 });
  });

  it("a pinned form gets a slot; a removed panel goes back to the chat", () => {
    const b = boardOf(msgs, { ...EMPTY_BOARD_PREFS, pinned: ["ask"], removed: ["c:sys"] });
    expect(b.panels.map((p) => p.key)).toEqual(["p:revenue", "c:ask"]);
    expect(b.panels[1]).toMatchObject({ interactive: true, pinned: true });
    expect(b.cardSlots.has("sys")).toBe(false);
  });

  it("follows the user's order, then age, and their width choice over the heuristic", () => {
    const b = boardOf(msgs, { ...EMPTY_BOARD_PREFS, order: ["c:sys"], wide: { "c:sys": true } });
    expect(b.panels.map((p) => p.key)).toEqual(["c:sys", "p:revenue"]);
    expect(b.panels[0].wide).toBe(true);
    expect(b.panels[1].wide).toBe(false);
  });

  it("ignores a refused card and an older call whose id was reused", () => {
    const refused = { ...assistant(1, [{ id: "x", input: display("X") }]) };
    refused.toolCalls![0].resultText = "UI_INVALID: nope";
    expect(boardOf([refused]).panels).toHaveLength(0);
    const reused = [assistant(1, [{ id: "r", input: display("Old") }]), assistant(2, [{ id: "r", input: form("New") }])];
    expect(boardOf(reused).panels).toHaveLength(0);
  });
});

describe("prefersWide", () => {
  it("gives trends, tables and grids room, not a small bar chart", () => {
    const spec = (components: unknown[]) => ({ title: "t", components }) as never;
    expect(prefersWide(spec([{ type: "chart", kind: "line", labels: ["a"], series: [] }]))).toBe(true);
    expect(prefersWide(spec([{ type: "chart", kind: "bar", labels: ["a", "b"], series: [] }]))).toBe(false);
    expect(prefersWide(spec([{ type: "section", children: [{ type: "table", columns: [], rows: [] }] }]))).toBe(true);
    expect(prefersWide(spec([{ type: "stat", label: "x", value: "1" }]))).toBe(false);
  });
});

describe("reordering", () => {
  it("moveKey moves by a step and clamps at the ends", () => {
    expect(moveKey(["a", "b", "c"], "a", 1)).toEqual(["b", "a", "c"]);
    expect(moveKey(["a", "b", "c"], "a", -1)).toEqual(["a", "b", "c"]);
    expect(moveKey(["a", "b", "c"], "c", 5)).toEqual(["a", "b", "c"]);
  });
  it("dropKey lands after a later target and before an earlier one", () => {
    expect(dropKey(["a", "b", "c"], "a", "c")).toEqual(["b", "c", "a"]);
    expect(dropKey(["a", "b", "c"], "c", "a")).toEqual(["c", "a", "b"]);
    expect(dropKey(["a", "b"], "a", "a")).toEqual(["a", "b"]);
  });
});

describe("boardStatus", () => {
  it("reports waiting approvals first, then a live turn's tool, then standby", () => {
    const pending = { id: 1, role: "assistant", content: "", state: "done", approvals: [{ id: "ap", status: "pending" }] } as unknown as Message;
    expect(boardStatus([pending], true)).toEqual({ kind: "approval", count: 1 });
    const running = assistant(2, [{ id: "t", input: "{}", state: "pending" }]);
    running.toolCalls![0].name = "web_search";
    expect(boardStatus([running], true)).toEqual({ kind: "working", label: "web_search" });
    expect(boardStatus([running], false)).toEqual({ kind: "standby" });
  });
});
