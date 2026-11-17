import { afterEach, describe, expect, it, vi } from "vitest";
import { readPatrolBarHidden, writePatrolBarHidden } from "./patrolBarPrefs";

afterEach(() => {
  vi.restoreAllMocks();
  window.localStorage.clear();
});

describe("patrolBarPrefs", () => {
  it("remembers a hidden bar per conversation and forgets it when shown again", () => {
    expect(readPatrolBarHidden("conv-a")).toBe(false);
    writePatrolBarHidden("conv-a", true);
    expect(readPatrolBarHidden("conv-a")).toBe(true);
    expect(readPatrolBarHidden("conv-b")).toBe(false);
    writePatrolBarHidden("conv-a", false);
    expect(readPatrolBarHidden("conv-a")).toBe(false);
    expect(window.localStorage.length).toBe(0);
  });

  it("reads as shown when storage throws", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("blocked");
    });
    expect(readPatrolBarHidden("conv-a")).toBe(false);
  });
});
