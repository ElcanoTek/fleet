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
const tagCatalogueMock = vi.fn();

vi.mock("@/app/shared/lib/orchestratorApi", () => ({
  orchestratorApi: {
    stats: () => statsMock(),
    tasks: (qs: string) => tasksMock(qs),
    tagCatalogue: () => tagCatalogueMock(),
  },
}));

import { useDashboardData } from "./useDashboardData";

afterEach(() => {
  cleanup();
  taskDeferreds.clear();
  vi.restoreAllMocks();
});

// The catalogue fetch fires on activation in every test here; default it to an
// empty list so a test that does not care about tags does not have to.
tagCatalogueMock.mockResolvedValue([]);

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

// ── Tag filter (#212) ────────────────────────────────────────────────────
// Tags AND together server-side, so every selected tag has to reach the
// request. `tag` is the one repeatable parameter in the query, and the
// obvious URLSearchParams.set() would have kept only the last of them —
// silently widening "carrying BOTH of these" to "carrying this one".
describe("useDashboardData tag filter", () => {
  function settle() {
    statsMock.mockResolvedValue({});
    tasksMock.mockResolvedValue({ data: [], total: 0 });
  }

  it("sends every selected tag, not just the last", async () => {
    settle();
    const { result } = renderHook(() => useDashboardData(true));
    await waitFor(() => expect(tasksMock).toHaveBeenCalled());

    act(() => result.current.setFilters({ tags: ["ops", "urgent"] }));
    await waitFor(() => {
      const qs = new URLSearchParams(tasksMock.mock.calls.at(-1)![0] as string);
      expect(qs.getAll("tag")).toEqual(["ops", "urgent"]);
    });
  });

  it("sends no tag parameter when none is selected", async () => {
    settle();
    renderHook(() => useDashboardData(true));
    await waitFor(() => expect(tasksMock).toHaveBeenCalled());
    const qs = new URLSearchParams(tasksMock.mock.calls.at(-1)![0] as string);
    expect(qs.getAll("tag")).toEqual([]);
  });

  it("offers the catalogue unioned with the tags on the listed tasks", async () => {
    statsMock.mockResolvedValue({});
    // A tag created after the catalogue was fetched: it is on a listed task
    // but not in the catalogue, and must still be selectable — that union is
    // what makes fetching the catalogue once per activation safe.
    tagCatalogueMock.mockResolvedValue([{ tag: "ops", task_count: 4 }]);
    tasksMock.mockResolvedValue({
      data: [{ id: "a", prompt: "p", tags: ["ops", "fresh"] }],
      total: 1,
    });

    const { result } = renderHook(() => useDashboardData(true));
    await waitFor(() => expect(result.current.tagOptions).toContain("fresh"));
    expect(result.current.tagOptions).toContain("ops");
    // Catalogue first (busiest first), then whatever the page added.
    expect(result.current.tagOptions).toEqual(["ops", "fresh"]);
  });

  it("keeps working when the catalogue fetch fails", async () => {
    settle();
    tagCatalogueMock.mockRejectedValue(new Error("boom"));
    const { result } = renderHook(() => useDashboardData(true));
    await waitFor(() => expect(tasksMock).toHaveBeenCalled());
    // No unhandled rejection, no error surfaced to the board: the filter loses
    // its suggestions, the dashboard keeps working.
    expect(result.current.tagOptions).toEqual([]);
    expect(result.current.error).toBeNull();
  });

  it("clearFilters drops the tags too", async () => {
    settle();
    const { result } = renderHook(() => useDashboardData(true));
    await waitFor(() => expect(tasksMock).toHaveBeenCalled());
    act(() => result.current.setFilters({ tags: ["ops"] }));
    await waitFor(() => expect(result.current.filters.tags).toEqual(["ops"]));
    act(() => result.current.clearFilters());
    expect(result.current.filters.tags).toEqual([]);
  });
});

// The catalogue used to be fetched once per activation. `active` stays true for
// the whole signed-in session, so a tag created afterwards on a task that is
// not on the current page never reached the dropdown until a full reload. The
// fix is a TTL, not a fetch on every reload: the catalogue is a GROUP BY over
// every task's tag array and paying for it every 30s buys nothing.
describe("useDashboardData tag catalogue freshness", () => {
  it("refetches on a reload once the catalogue is stale, and not before", async () => {
    statsMock.mockResolvedValue({});
    tasksMock.mockResolvedValue({ data: [], total: 0 });
    tagCatalogueMock.mockResolvedValue([{ tag: "ops", task_count: 1 }]);

    const now = vi.spyOn(Date, "now");
    let clock = 1_000_000;
    now.mockImplementation(() => clock);

    const { result } = renderHook(() => useDashboardData(true));
    await waitFor(() => expect(tagCatalogueMock).toHaveBeenCalledTimes(1));

    // A reload inside the TTL must not re-fetch it.
    clock += 60_000;
    await act(async () => {
      await result.current.reload();
    });
    expect(tagCatalogueMock).toHaveBeenCalledTimes(1);

    // Past the TTL, the next reload picks up the new tag.
    clock += 5 * 60_000;
    tagCatalogueMock.mockResolvedValue([
      { tag: "ops", task_count: 1 },
      { tag: "created-later", task_count: 1 },
    ]);
    await act(async () => {
      await result.current.reload();
    });
    await waitFor(() => expect(tagCatalogueMock).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(result.current.tagOptions).toContain("created-later"));

    now.mockRestore();
  });
});
