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

describe("useDashboardData adaptive refresh", () => {
  it("polls at 5s while work is in flight and 30s when idle", async () => {
    tasksMock.mockResolvedValue({ data: [], total: 0 });

    statsMock.mockResolvedValue({ pending_tasks: 0, running_tasks: 2 });
    const { result } = renderHook(() => useDashboardData(true));
    await act(async () => {
      await Promise.resolve();
    });
    await waitFor(() => expect(result.current.stats).not.toBeNull());
    expect(result.current.refreshSeconds).toBe(5);

    // Work drains; the next reload relaxes the cadence.
    statsMock.mockResolvedValue({ pending_tasks: 0, running_tasks: 0 });
    await act(async () => {
      await result.current.reload();
    });
    expect(result.current.refreshSeconds).toBe(30);

    // Active agents alone also keep it hot.
    statsMock.mockResolvedValue({ active_agents: 1 });
    await act(async () => {
      await result.current.reload();
    });
    expect(result.current.refreshSeconds).toBe(5);
  });

  it("refetches when the tab becomes visible again", async () => {
    tasksMock.mockResolvedValue({ data: [], total: 0 });
    statsMock.mockResolvedValue({});
    renderHook(() => useDashboardData(true));
    await act(async () => {
      await Promise.resolve();
    });
    const before = statsMock.mock.calls.length;
    await act(async () => {
      window.dispatchEvent(new Event("focus"));
      await Promise.resolve();
    });
    expect(statsMock.mock.calls.length).toBeGreaterThan(before);
  });
});

describe("useDashboardData paging", () => {
  it("snaps back to page 1 when the page size changes", async () => {
    tasksMock.mockResolvedValue({ data: [], total: 100 });
    statsMock.mockResolvedValue({});
    const { result } = renderHook(() => useDashboardData(true));
    await act(async () => {
      await Promise.resolve();
    });

    await act(async () => {
      result.current.setPage(5);
    });
    expect(result.current.page).toBe(5);

    // Page 5 of a 20-per-page list is past the end of a 50-per-page one
    // ("Page 5 of 2", empty table) — a size change re-buckets from the top.
    await act(async () => {
      result.current.setPageSize(50);
    });
    expect(result.current.pageSize).toBe(50);
    expect(result.current.page).toBe(1);
    await waitFor(() =>
      expect(tasksMock).toHaveBeenLastCalledWith(expect.stringContaining("limit=50&offset=0")),
    );
  });

  it("bumps refreshNonce once per completed reload", async () => {
    tasksMock.mockResolvedValue({ data: [], total: 0 });
    statsMock.mockResolvedValue({});
    const { result } = renderHook(() => useDashboardData(true));
    await act(async () => {
      await Promise.resolve();
    });
    await waitFor(() => expect(result.current.refreshNonce).toBe(1));
    await act(async () => {
      await result.current.reload();
    });
    expect(result.current.refreshNonce).toBe(2);
  });
});
