import { afterEach, describe, expect, it, vi } from "vitest";
import { renderHook, act, waitFor, cleanup } from "@testing-library/react";

// #126: a slower EARLIER reload (e.g. the prior search term) must not overwrite
// the results of a newer one. The reload() run-id guard enforces this; here we
// resolve a superseded reload AFTER its successor and assert the stale result
// is discarded.

// Controllable deferred per tasks() call, keyed by the `q` query param so the
// test can resolve calls out of order.
const taskDeferreds = new Map<string, { resolve: (v: unknown) => void }>();
function deferred() {
  let resolve!: (v: unknown) => void;
  const promise = new Promise((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

const statsMock = vi.fn();
const tasksMock = vi.fn();

vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: {
    stats: () => statsMock(),
    tasks: (qs: string) => tasksMock(qs),
  },
}));

import { useDashboardData } from "./useDashboardData";

afterEach(() => {
  cleanup();
  taskDeferreds.clear();
  vi.restoreAllMocks();
});

function qOf(qs: string): string {
  return new URLSearchParams(qs).get("q") ?? "";
}

describe("useDashboardData run-id supersession", () => {
  it("discards a superseded reload that resolves after a newer one", async () => {
    statsMock.mockResolvedValue({});
    // Each tasks() call gets its own deferred, registered by query value.
    tasksMock.mockImplementation((qs: string) => {
      const d = deferred();
      taskDeferreds.set(qOf(qs), { resolve: d.resolve });
      return d.promise;
    });

    const { result } = renderHook(() => useDashboardData(true));

    // Let the mount kickoff (queueMicrotask) start its reload.
    await act(async () => {
      await Promise.resolve();
    });

    // Two reloads in flight: q=a (older/slower) then q=ab (newer).
    await act(async () => {
      result.current.setFilters({ query: "a" });
      await Promise.resolve();
    });
    await act(async () => {
      result.current.setFilters({ query: "ab" });
      await Promise.resolve();
    });

    // Both search reloads should have registered their tasks() calls.
    await waitFor(() => {
      expect(taskDeferreds.has("a")).toBe(true);
      expect(taskDeferreds.has("ab")).toBe(true);
    });

    // The NEWER reload (q=ab) resolves first and writes its result.
    await act(async () => {
      taskDeferreds.get("ab")!.resolve({ data: [{ id: "newer", prompt: "ab" }], total: 1 });
      await Promise.resolve();
    });
    await waitFor(() => expect(result.current.tasks).toEqual([{ id: "newer", prompt: "ab" }]));

    // The superseded reload (q=a) resolves LATER — its stale result must be
    // discarded by the run-id guard, leaving the newer state intact.
    await act(async () => {
      taskDeferreds.get("a")!.resolve({ data: [{ id: "stale", prompt: "a" }], total: 1 });
      // Also drain the mount reload so no dangling promise.
      taskDeferreds.get("")?.resolve({ data: [{ id: "mount", prompt: "" }], total: 0 });
      await Promise.resolve();
    });

    expect(result.current.tasks).toEqual([{ id: "newer", prompt: "ab" }]);
  });
});
