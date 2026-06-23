"use client";

import { useCallback, useEffect, useState } from "react";
import {
  orchestratorApi,
  type DashboardStats,
  type Node,
  type Task,
} from "@/app/shared/lib/orchestratorApi";

// useDashboardData drives the orchestrator dashboard: stats + nodes + a
// filtered/paginated task list, with 30s auto-refresh. React port of moc's
// dashboard.js loadDashboard()/loadStats()/loadNodes()/loadTasks() +
// startAutoRefresh().

export type TaskFilters = {
  status: string;
  query: string;
  scheduledOnly: boolean;
  completedToday: boolean;
  completedStatus: string;
  createdBy: string;
};

const EMPTY_FILTERS: TaskFilters = {
  status: "",
  query: "",
  scheduledOnly: false,
  completedToday: false,
  completedStatus: "",
  createdBy: "",
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
  return p.toString();
}

export type UseDashboardData = {
  stats: DashboardStats | null;
  nodes: Node[];
  tasks: Task[];
  total: number;
  loading: boolean;
  filters: TaskFilters;
  page: number;
  pageSize: number;
  setFilters: (next: Partial<TaskFilters>) => void;
  clearFilters: () => void;
  setPage: (page: number) => void;
  setPageSize: (size: number) => void;
  reload: () => Promise<void>;
};

export function useDashboardData(active: boolean): UseDashboardData {
  const [stats, setStats] = useState<DashboardStats | null>(null);
  const [nodes, setNodes] = useState<Node[]>([]);
  const [tasks, setTasks] = useState<Task[]>([]);
  const [total, setTotal] = useState(0);
  // Lazy-init to `active`: when the dashboard mounts active we begin in the
  // loading state on the first render, so we never have to flip loading true
  // synchronously inside the mount effect (which would trip
  // react-hooks/set-state-in-effect). reload() flips it true again from a
  // deferred kickoff / interval / imperative call — all off the effect's
  // synchronous phase.
  const [loading, setLoading] = useState(active);
  const [filters, setFiltersState] = useState<TaskFilters>(EMPTY_FILTERS);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);

  // reload depends on the current filters/page/size, so it changes when they
  // do. The effects below re-run on that identity change, which is exactly the
  // "refetch when filters move" behavior — no refs needed.
  const reload = useCallback(async () => {
    setLoading(true);
    const qs = buildTaskQuery(filters, page, pageSize);
    const results = await Promise.allSettled([
      orchestratorApi.stats(),
      orchestratorApi.nodes(),
      orchestratorApi.tasks(qs),
    ]);
    if (results[0].status === "fulfilled") setStats(results[0].value);
    if (results[1].status === "fulfilled") setNodes(results[1].value.data ?? []);
    if (results[2].status === "fulfilled") {
      setTasks(results[2].value.data ?? []);
      setTotal(results[2].value.total ?? 0);
    }
    setLoading(false);
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

  // 30s auto-refresh, matching moc.
  useEffect(() => {
    if (!active) return;
    const id = setInterval(() => void reload(), 30000);
    return () => clearInterval(id);
  }, [active, reload]);

  const setFilters = useCallback((next: Partial<TaskFilters>) => {
    setFiltersState((prev) => ({ ...prev, ...next }));
    setPage(1);
  }, []);

  const clearFilters = useCallback(() => {
    setFiltersState(EMPTY_FILTERS);
    setPage(1);
  }, []);

  return {
    stats,
    nodes,
    tasks,
    total,
    loading,
    filters,
    page,
    pageSize,
    setFilters,
    clearFilters,
    setPage,
    setPageSize,
    reload,
  };
}
