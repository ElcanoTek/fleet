import { afterEach, describe, expect, it, vi } from "vitest";
import { EMPTY_BOARD_PREFS } from "./board";
import { MAX_REMEMBERED, readBoardPrefs, readDashboardView, writeBoardPrefs, writeDashboardView } from "./prefs";

afterEach(() => {
  window.localStorage.clear();
  vi.restoreAllMocks();
});

describe("dashboard view preference", () => {
  it("is remembered per conversation and bounded", () => {
    writeDashboardView("c1", true);
    expect(readDashboardView("c1")).toBe(true);
    expect(readDashboardView("c2")).toBe(false);
    expect(readDashboardView(null)).toBe(false);
    writeDashboardView("c1", false);
    expect(readDashboardView("c1")).toBe(false);
    for (let i = 0; i < MAX_REMEMBERED + 5; i++) writeDashboardView(`x${i}`, true);
    expect(readDashboardView("x0")).toBe(false);
    expect(readDashboardView(`x${MAX_REMEMBERED + 4}`)).toBe(true);
  });
});

describe("board prefs", () => {
  it("round-trip and drop malformed entries", () => {
    writeBoardPrefs("c1", { pinned: ["a"], removed: ["c:b"], order: ["p:x"], wide: { "p:x": true } });
    expect(readBoardPrefs("c1")).toEqual({ pinned: ["a"], removed: ["c:b"], order: ["p:x"], wide: { "p:x": true } });
    window.localStorage.setItem(
      "fleet.dashboard.board.v2",
      JSON.stringify([{ id: "c2", prefs: { pinned: [1, "ok"], wide: { k: "yes", ok: true } }, at: 1 }, { bogus: 1 }]),
    );
    expect(readBoardPrefs("c2")).toEqual({ pinned: ["ok"], removed: [], order: [], wide: { ok: true } });
  });

  it("treats a conversation id as data, never as an object key", () => {
    writeBoardPrefs("__proto__", { ...EMPTY_BOARD_PREFS, pinned: ["x"] });
    expect(({} as Record<string, unknown>).pinned).toBeUndefined();
    expect(readBoardPrefs("__proto__").pinned).toEqual(["x"]);
    window.localStorage.setItem(
      "fleet.dashboard.board.v2",
      '[{"id":"c3","at":1,"prefs":{"wide":{"__proto__":true,"p:a":false}}}]',
    );
    const prefs = readBoardPrefs("c3");
    expect(prefs.wide).toEqual({ "p:a": false });
    expect(Object.getPrototypeOf(prefs.wide)).toBe(Object.prototype);
  });

  it("degrades to defaults when storage throws", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    expect(readBoardPrefs("c1")).toEqual(EMPTY_BOARD_PREFS);
    expect(() => writeBoardPrefs("c1", EMPTY_BOARD_PREFS)).not.toThrow();
    expect(() => writeDashboardView("c1", true)).not.toThrow();
    expect(readDashboardView("c1")).toBe(false);
  });
});
