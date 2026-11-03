import { describe, expect, it } from "vitest";
import type { Message } from "../history";
import { deriveGenUiState } from "../genui/transcript";
import { panelDomId } from "./context";
import { boardManifest, boardStatus, deriveBoard, MAX_MANIFEST_PANELS, MAX_MANIFEST_TITLE, dropKey, EMPTY_BOARD_PREFS, moveKey, prefersWide, type BoardPrefs } from "./board";

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

  it("a pinned card refreshed with replaces + a panel key keeps its place, version and layout", () => {
    const pinnedFlow = [
      assistant(1, [
        { id: "a", input: display("A", { panel: "a" }) },
        { id: "f", input: form("Follow up") },
        { id: "z", input: display("Z") },
      ]),
      assistant(2, [{ id: "f2", input: display("Follow up (sent)", { replaces: "f", panel: "follow" }) }]),
    ];
    const prefs = { ...EMPTY_BOARD_PREFS, pinned: ["f"], order: ["c:f", "p:a", "c:z"], wide: { "c:f": true } };
    const b = boardOf(pinnedFlow, prefs);
    expect(b.panels.map((p) => p.key)).toEqual(["p:follow", "p:a", "c:z"]);
    expect(b.panels[0]).toMatchObject({ cardId: "f2", version: 2, pinned: true, wide: true, panelKey: "follow" });
    expect(b.cardSlots.get("f")).toBe("p:follow");
    // Taking it off the board under its old key still takes it off.
    expect(boardOf(pinnedFlow, { ...prefs, removed: ["c:f"] }).panels.map((p) => p.key)).toEqual(["p:a", "c:z"]);
    expect(boardManifest(b.panels)[0]).toEqual({ title: "Follow up (sent)", card_id: "f2", panel: "follow", pinned: true });
  });

  it("a refinement that names a taken panel key merges its old slot into that panel", () => {
    const b = boardOf([
      assistant(1, [
        { id: "a", input: display("A") },
        { id: "b", input: display("B", { panel: "x" }) },
      ]),
      assistant(2, [{ id: "c", input: display("C", { replaces: "a", panel: "x" }) }]),
    ]);
    expect(b.panels.map((p) => [p.key, p.cardId])).toEqual([["p:x", "c"]]);
    expect(b.cardSlots.get("a")).toBe("p:x");
  });

  it("refining a stale version of a panel takes nothing over", () => {
    const b = boardOf([
      assistant(1, [{ id: "a", input: display("A", { panel: "x" }) }]),
      assistant(2, [{ id: "b", input: display("B", { panel: "x" }) }]),
      assistant(3, [{ id: "c", input: display("C (from A)", { replaces: "a" }) }]),
    ]);
    const x = b.panels.find((p) => p.key === "p:x");
    expect(x?.cardId).toBe("b");
    // C is display-only, so it gets a panel of its own instead.
    expect(b.panels.map((p) => p.key)).toEqual(["p:x", "c:c"]);
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

describe("manifest and ids", () => {
  it("bounds the manifest to the server's limits", () => {
    const many = Array.from({ length: 40 }, (_, i) =>
      assistant(i + 1, [{ id: `c${i}`, input: display("T".repeat(500)) }]),
    );
    const m = boardManifest(boardOf(many).panels);
    expect(m).toHaveLength(MAX_MANIFEST_PANELS);
    expect(Array.from(m[0].title)).toHaveLength(MAX_MANIFEST_TITLE);
  });

  it("panelDomId never maps two slot keys to one id", () => {
    const keys = ["c:call:1", "c:call/1", "c:call_1", "c:call_3a_1", "p:a-b"];
    expect(new Set(keys.map(panelDomId)).size).toBe(keys.length);
    expect(panelDomId("p:rev")).toMatch(/^dash-panel-[A-Za-z0-9_-]+$/);
  });

  it("a reused card id in a panel is a new occurrence", () => {
    const b = boardOf([
      assistant(1, [{ id: "r", input: display("Rev", { panel: "rev" }) }]),
      assistant(2, [{ id: "r", input: display("Rev 2", { panel: "rev" }) }]),
    ]);
    expect(b.panels[0]).toMatchObject({ cardId: "r", occurrence: 1, spec: expect.objectContaining({ title: "Rev 2" }) });
  });
});
