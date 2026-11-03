import { act, renderHook } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { EMPTY_BOARD_PREFS } from "./board";
import { dashboardViewAfterSwitch, MAX_REMEMBERED, readBoardPrefs, useBoardPrefs, readDashboardView, writeBoardPrefs, writeDashboardView } from "./prefs";

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

describe("dashboardViewAfterSwitch", () => {
  it("keeps the view for a draft and a draft becoming a conversation, restores it otherwise", () => {
    writeDashboardView("saved-on", true);
    // A new draft keeps whatever view you were in.
    expect(dashboardViewAfterSwitch("conv-a", null, true, "pending")).toBe(true);
    // The draft's first send: pending:<n> → its real id keeps dashboard view.
    expect(dashboardViewAfterSwitch("pending:3", "new-conv", true, "pending")).toBe(true);
    // Opening an existing conversation from a blank chat restores ITS view.
    expect(dashboardViewAfterSwitch("pending", "plain-conv", true, "pending")).toBe(false);
    expect(dashboardViewAfterSwitch("pending", "saved-on", false, "pending")).toBe(true);
    // Conversation to conversation: each keeps its own.
    expect(dashboardViewAfterSwitch("saved-on", "plain-conv", true, "pending")).toBe(false);
    expect(dashboardViewAfterSwitch("plain-conv", "saved-on", false, "pending")).toBe(true);
    // Share before the first send creates the conversation straight from the
    // blank slot: still this draft, so the view stays on.
    expect(dashboardViewAfterSwitch("pending", "shared-new", true, "pending", true)).toBe(true);
    // …but only for that switch: coming back later from another chat is
    // ordinary navigation, even while the marker is still set.
    expect(dashboardViewAfterSwitch("plain-conv", "shared-new", true, "pending", true)).toBe(false);
  });
});

describe("useBoardPrefs", () => {
  const saved = { pinned: ["x"], removed: [], order: ["p:k"], wide: {} };

  it("loads a conversation's own layout when opened from a blank chat", () => {
    writeBoardPrefs("existing", saved);
    const { result, rerender } = renderHook(({ key, id }) => useBoardPrefs(key, id, "pending"), {
      initialProps: { key: "pending", id: null as string | null },
    });
    rerender({ key: "existing", id: "existing" });
    expect(result.current[0]).toEqual(saved);
  });

  it("starts a new draft empty when leaving a conversation", () => {
    writeBoardPrefs("existing", saved);
    const { result, rerender } = renderHook(({ key, id }) => useBoardPrefs(key, id, "pending"), {
      initialProps: { key: "existing", id: "existing" as string | null },
    });
    expect(result.current[0]).toEqual(saved);
    rerender({ key: "pending", id: null });
    expect(result.current[0]).toEqual({ pinned: [], removed: [], order: [], wide: {} });
    // …and the conversation's own layout is untouched.
    expect(readBoardPrefs("existing")).toEqual(saved);
  });

  it("carries a draft's choices onto a conversation Share created from it", () => {
    const { result, rerender } = renderHook(({ key, id, created }) => useBoardPrefs(key, id, "pending", created), {
      initialProps: { key: "pending", id: null as string | null, created: false },
    });
    act(() => result.current[1]((p) => ({ ...p, order: ["c:q"] })));
    rerender({ key: "shared-new", id: "shared-new", created: true });
    expect(result.current[0].order).toEqual(["c:q"]);
  });

  it("carries a draft's choices onto the id it gets", () => {
    const { result, rerender } = renderHook(({ key, id }) => useBoardPrefs(key, id, "pending"), {
      initialProps: { key: "pending", id: null as string | null },
    });
    act(() => result.current[1]((p) => ({ ...p, order: ["c:z"] })));
    // blank chat → its per-submission pending key → its id
    rerender({ key: "pending:1", id: null });
    rerender({ key: "new-conv", id: "new-conv" });
    expect(result.current[0].order).toEqual(["c:z"]);
    expect(readBoardPrefs("new-conv").order).toEqual(["c:z"]);
  });
});
