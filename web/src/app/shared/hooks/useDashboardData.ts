"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  orchestratorApi,
  type DashboardStats,
  type Task,
} from "@/app/shared/lib/orchestratorApi";

// useDashboardData drives the orchestrator dashboard: stats + a
// filtered/paginated task list, with 30s auto-refresh. React port of moc's
// dashboard.js loadDashboard()/loadStats()/loadTasks() + startAutoRefresh().

export type TaskFilters = {
  status: string;
  query: string;
  scheduledOnly: boolean;
  completedToday: boolean;
  completedStatus: string;
  createdBy: string;
  // Tags narrow to tasks carrying ALL of them (#212) — the server ANDs them,
  // so each tag you add makes the board smaller, never larger.
  tags: string[];
};

// How long a fetched tag catalogue is considered fresh. See the effect that
// uses it for why this is neither "once" nor "every reload".
const TAG_CATALOGUE_TTL_MS = 5 * 60 * 1000;

const EMPTY_FILTERS: TaskFilters = {
  status: "",
  query: "",
  scheduledOnly: false,
  completedToday: false,
  completedStatus: "",
  createdBy: "",
  tags: [],
};

function buildTaskQuery(filters: TaskFilters, page: number, pageSize: number): string {
  const p = new URLSearchParams();
  p.set("limit", String(pageSize));
  p.set("offset", String((page - 1) * pageSize));
  if (filters.status) p.set("status", filters.status);
  if (filters.query) p.set("q", filters.query);
  if (filters.scheduledOnly) p.set("scheduled_only", "true");
  if (filters.completedToday) {
    p.set("completed_today", "true");
    if (filters.completedStatus) p.set("completed_status", filters.completedStatus);
  }
  if (filters.createdBy) p.set("created_by", filters.createdBy);
  // append, not set: `tag` is repeatable and every value has to survive the
  // trip, since dropping one widens the result instead of narrowing it.
  for (const tag of filters.tags) p.append("tag", tag);
  return p.toString();
}

export type UseDashboardData = {
  stats: DashboardStats | null;
  tasks: Task[];
  total: number;
  loading: boolean;
  // Why the LAST task-list load produced nothing usable (null when it
  // succeeded). The table shows this instead of "No tasks created yet": with
  // Promise.allSettled swallowing the rejection, a backend outage used to be
  // indistinguishable from an empty account.
  error: string | null;
  filters: TaskFilters;
  // Tags the filter can offer: the deployment catalogue unioned with the tags
  // on the listed tasks. Catalogue order (busiest first) is preserved.
  tagOptions: string[];
  page: number;
  pageSize: number;
  setFilters: (next: Partial<TaskFilters>) => void;
  clearFilters: () => void;
  setPage: (page: number) => void;
  setPageSize: (size: number) => void;
  reload: () => Promise<void>;
  // Current auto-refresh cadence in seconds (5 while work is in flight, 30
  // when idle) so the UI can say what it actually does.
  refreshSeconds: number;
  // Increments once per completed reload (mount, interval, focus, manual).
  // Pass it as a dep to anything that fetches its own view of the task list
  // so it refreshes in step with the table.
  refreshNonce: number;
};

export function useDashboardData(active: boolean): UseDashboardData {
  const [stats, setStats] = useState<DashboardStats | null>(null);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [total, setTotal] = useState(0);
  // Lazy-init to `active`: when the dashboard mounts active we begin in the
  // loading state on the first render, so we never have to flip loading true
  // synchronously inside the mount effect (which would trip
  // react-hooks/set-state-in-effect). reload() flips it true again from a
  // deferred kickoff / interval / imperative call — all off the effect's
  // synchronous phase.
  const [loading, setLoading] = useState(active);
  const [error, setError] = useState<string | null>(null);
  const [filters, setFiltersState] = useState<TaskFilters>(EMPTY_FILTERS);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSizeState] = useState(20);
  // Bumped once per completed reload so siblings that fetch their own slice
  // of the task list (SleepingTasks) can refetch on the dashboard's cadence
  // instead of once at mount.
  const [refreshNonce, setRefreshNonce] = useState(0);
  // The deployment's tags, for the board's tag filter. Fetched ONCE per
  // activation rather than on the 30s refresh cadence: the catalogue is a
  // GROUP BY over every task's tag array, it changes only when someone retags
  // something, and paying for it every half-minute on every open dashboard
  // buys nothing. tagOptions below closes the resulting gap.
  const [tagCatalogue, setTagCatalogue] = useState<string[]>([]);
  // When the catalogue was last requested, for the TTL below. 0 = never.
  const catalogueFetchedAt = useRef(0);
  // Monotonic id stamped on each reload so a superseded (slower, older) reload
  // cannot overwrite newer state — see reload().
  const runIdRef = useRef(0);
  // Monotonic generation counter for tag catalogue requests. Incremented on
  // each fetch start and on deactivation/unmount so in-flight requests cannot
  // overwrite newer state across activations or reload-triggered retries.
  const catalogueGenRef = useRef(0);

  // Invalidate any in-flight catalogue request whenever the hook deactivates
  // or unmounts, and reset catalogueFetchedAt so that reactivating begins with
  // a fresh fetch rather than waiting out the remainder of the TTL.
  useEffect(() => {
    const genRef = catalogueGenRef;
    const fetchedAtRef = catalogueFetchedAt;
    if (!active) {
      genRef.current++;
      fetchedAtRef.current = 0;
    }
    return () => {
      genRef.current++;
      fetchedAtRef.current = 0;
    };
  }, [active]);

  // reload depends on the current filters/page/size, so it changes when they
  // do. The effects below re-run on that identity change, which is exactly the
  // "refetch when filters move" behavior — no refs needed.
  const reload = useCallback(async () => {
    // Monotonic run-id supersession: a slower earlier reload (e.g. the prior
    // search term) must not overwrite the newer one's results. Each call claims
    // the next id; after the awaits, a call whose id is no longer current bails
    // out and writes nothing — including not flipping loading off for the run
    // that superseded it.
    const runId = ++runIdRef.current;
    setLoading(true);
    const qs = buildTaskQuery(filters, page, pageSize);
    const results = await Promise.allSettled([
      orchestratorApi.stats(),
      orchestratorApi.tasks(qs),
    ]);
    if (runId !== runIdRef.current) return;
    if (results[0].status === "fulfilled") setStats(results[0].value);
    if (results[1].status === "fulfilled") {
      setTasks(results[1].value.data ?? []);
      setTotal(results[1].value.total ?? 0);
      setError(null);
    } else {
      const reason = results[1].reason;
      setError(reason instanceof Error ? reason.message : String(reason));
    }
    setLoading(false);
    setRefreshNonce((n) => n + 1);
  }, [filters, page, pageSize]);

  // Fetch on mount/filters/page change and whenever reload's identity changes.
  // The kickoff is deferred to a microtask so reload's synchronous
  // setLoading(true) runs outside the effect's synchronous phase (otherwise
  // react-hooks/set-state-in-effect flags the cascading render); a guard skips
  // the call if deps change or we unmount before the microtask runs.
  useEffect(() => {
    if (!active) return;
    let cancelled = false;
    queueMicrotask(() => {
      if (!cancelled) void reload();
    });
    return () => {
      cancelled = true;
    };
  }, [active, reload]);

  // Adaptive auto-refresh: 5s while anything is in flight (pending/running
  // tasks or active agents) so short-lived statuses are actually visible,
  // relaxing to 30s when idle. The interval re-arms when the cadence flips.
  const fastPoll =
    !!stats &&
    ((stats.pending_tasks ?? 0) > 0 ||
      (stats.running_tasks ?? 0) > 0 ||
      (stats.active_agents ?? 0) > 0);
  const refreshSeconds = fastPoll ? 5 : 30;
  useEffect(() => {
    if (!active) return;
    const id = setInterval(() => void reload(), refreshSeconds * 1000);
    return () => clearInterval(id);
  }, [active, reload, refreshSeconds]);

  // Refetch when the tab regains focus/visibility — a backgrounded dashboard
  // comes back current instead of up to 30s stale.
  useEffect(() => {
    if (!active) return;
    const onVisible = () => {
      if (document.visibilityState === "visible") void reload();
    };
    window.addEventListener("focus", onVisible);
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      window.removeEventListener("focus", onVisible);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [active, reload]);

  // Refreshed off the ordinary reload cadence, but at most once per
  // TAG_CATALOGUE_TTL_MS. Once per activation was too rarely: `active` stays
  // true for the whole signed-in session, so a tag created afterwards on a
  // task that is not on the page in front of you never reached the dropdown
  // until a full reload. Every 30s, with the rest of the dashboard, would be
  // too often for a GROUP BY over every task's tag array that changes only
  // when somebody retags something. The TTL bounds both: one query per five
  // minutes per open dashboard, and a new tag is offered within five minutes
  // of being created — sooner if a task carrying it appears (see tagOptions).
  //
  // Freshness is checked on [active, refreshNonce], but in-flight request
  // supersession is governed by catalogueGenRef: a reload mid-flight does not
  // increment the generation, so the response is applied when it resolves.
  // When a response arrives with a mismatched generation, it is simply
  // discarded: either a newer request already owns the current stamp, or the
  // [active] effect already zeroed it on deactivation.
  useEffect(() => {
    if (!active) return;
    if (Date.now() - catalogueFetchedAt.current < TAG_CATALOGUE_TTL_MS) return;
    // Stamped before the request, not after: two reloads landing while one is
    // in flight must not each start their own.
    catalogueFetchedAt.current = Date.now();
    const gen = ++catalogueGenRef.current;
    orchestratorApi
      .tagCatalogue()
      .then((rows) => {
        if (gen !== catalogueGenRef.current) return;
        setTagCatalogue(rows.map((r) => r.tag));
      })
      // A missing catalogue costs the filter its suggestions, nothing more:
      // tagOptions still offers the tags on screen, and a tag already applied
      // keeps filtering. Failing the whole dashboard over it would be worse.
      // The stamp stands, so a failing endpoint is retried on the TTL rather
      // than on every reload.
      .catch(() => {});
  }, [active, refreshNonce]);

  // What the tag filter offers: the catalogue, plus any tag on a task
  // currently listed. The union is what closes the gap the TTL leaves — a tag
  // created since the last catalogue fetch is selectable immediately, without
  // waiting for the refresh, as soon as a task carrying it appears.
  const tagOptions = useMemo(() => {
    const seen = new Set(tagCatalogue);
    for (const task of tasks) for (const tag of task.tags ?? []) seen.add(tag);
    return [...seen];
  }, [tagCatalogue, tasks]);

  const setFilters = useCallback((next: Partial<TaskFilters>) => {
    setFiltersState((prev) => ({ ...prev, ...next }));
    setPage(1);
  }, []);

  const clearFilters = useCallback(() => {
    setFiltersState(EMPTY_FILTERS);
    setPage(1);
  }, []);

  // A new page size re-buckets the whole list, so the current page number is
  // meaningless under it: page 5 of a 20-per-page list is past the end at 50
  // per page ("Page 5 of 2", an empty table). Snap back to the first page, the
  // same way a filter change does.
  const setPageSize = useCallback((size: number) => {
    setPageSizeState(size);
    setPage(1);
  }, []);

  return {
    stats,
    tasks,
    total,
    loading,
    error,
    filters,
    tagOptions,
    page,
    pageSize,
    setFilters,
    clearFilters,
    setPage,
    setPageSize,
    reload,
    refreshSeconds,
    refreshNonce,
  };
}
