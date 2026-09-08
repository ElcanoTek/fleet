import { afterEach, describe, expect, it, vi } from "vitest";

import { allocMessageIds } from "./messageIds";

afterEach(() => {
  vi.useRealTimers();
});

describe("allocMessageIds", () => {
  it("never repeats an id while the clock stands still", () => {
    vi.useFakeTimers();
    vi.setSystemTime(1_700_000_000_000);
    const seen = new Set<number>();
    for (let i = 0; i < 1000; i++) {
      const id = allocMessageIds();
      expect(seen.has(id)).toBe(false);
      seen.add(id);
    }
  });

  it("reserves a consecutive pair so assistantId - 1 is the caller's own", () => {
    vi.useFakeTimers();
    vi.setSystemTime(1_700_000_000_000);
    const base = allocMessageIds(2);
    const next = allocMessageIds();
    // base and base+1 belong to the first caller; the next allocation starts
    // after both.
    expect(next).toBe(base + 2);
  });

  it("reads as a timestamp: never behind the clock, and monotonic across time jumps", () => {
    vi.useFakeTimers();
    vi.setSystemTime(1_700_000_000_000);
    const early = allocMessageIds();
    expect(early).toBeGreaterThanOrEqual(1_700_000_000_000);
    vi.setSystemTime(1_700_000_100_000);
    const later = allocMessageIds();
    expect(later).toBe(1_700_000_100_000);
    // A clock that goes BACKWARDS must not hand out an old id again.
    vi.setSystemTime(1_699_000_000_000);
    expect(allocMessageIds()).toBeGreaterThan(later);
  });
});
