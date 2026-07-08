"use client";

// Settings → Admin → Overview (fleet-unified settings pass): the operator's
// live single-pane view — system health from GET /api/admin/health-summary
// (polled every 10s, #301), the MCP catalog chips, and per-deployment usage
// aggregates from GET /api/admin/stats. Deliberately dependency-free — stat
// cards + chips, no external chart library. Power tools belong in real
// observability (Grafana, etc); this is a "is the box healthy, who's costing
// money" sanity check for the operator of a 10-20 user deployment.
//
// Gating: useIsAdmin is VISIBILITY only (members are bounced back to
// /settings); authorization stays server-side — both endpoints independently
// 403 non-admins, and those error paths render below.

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";

import { fetchStats, formatAgo, formatUSD, type UserStat } from "./lib";
import { ConnBadge } from "../ui/atoms";
import { AdminStats, ConnGroup, ConnGroupHead, SetSection, type AdminStat } from "../ui/panels";
import { useIsAdmin } from "../useIsAdmin";
import { Icon } from "@/app/shared/ui/Icon";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";

type HealthSummary = {
  fleet_version: string;
  uptime_seconds: number;
  db: { chat: string; pool_size: number; in_use: number; idle: number };
  workers: {
    queued_tasks: number;
    running_tasks: number;
    completed_today: number;
    failed_today: number;
  } | null;
  llm: { calls_today: number; cost_today_usd: number; avg_cost_per_call: number };
  mcp_servers: Array<{ name: string; enabled: boolean }>;
  conversations_active: number;
  sandbox_pool: { size: number; available: number } | null;
  memory_mb: number;
  goroutines: number;
};

const REFRESH_MS = 10_000;

function formatUptime(seconds: number): string {
  if (seconds <= 0) return "—";
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}

// healthItems maps the summary onto the design's ADMIN_HEALTH card order.
function healthItems(d: HealthSummary): AdminStat[] {
  const items: AdminStat[] = [
    { title: "Version", value: d.fleet_version, sub: `up ${formatUptime(d.uptime_seconds)}` },
    { title: "DB pool", value: `${d.db.in_use}/${d.db.pool_size}`, sub: `${d.db.idle} idle` },
    {
      title: "LLM today",
      value: `$${d.llm.cost_today_usd.toFixed(2)}`,
      sub: `${d.llm.calls_today} calls · $${d.llm.avg_cost_per_call.toFixed(3)}/call`,
    },
    { title: "Active chats", value: String(d.conversations_active) },
  ];
  if (d.workers) {
    // Single-box deploy: there are no separate worker nodes, so these are
    // task-throughput cards (what the scheduler is doing right now), not a
    // node count.
    items.push(
      {
        title: "Tasks",
        value: `${d.workers.running_tasks} running`,
        sub: `${d.workers.queued_tasks} queued`,
      },
      {
        title: "Tasks today",
        value: `${d.workers.completed_today} ✓`,
        sub: `${d.workers.failed_today} failed`,
      },
    );
  } else {
    items.push({ title: "Tasks", value: "—", sub: "scheduler stats unavailable" });
  }
  items.push(
    {
      title: "Sandbox pool",
      value: d.sandbox_pool ? `${d.sandbox_pool.available}/${d.sandbox_pool.size}` : "—",
      sub: d.sandbox_pool ? "ready / size" : "not configured",
    },
    { title: "Runtime", value: `${d.memory_mb} MB`, sub: `${d.goroutines} goroutines` },
  );
  return items;
}

export default function AdminOverviewPage() {
  const router = useRouter();
  const admin = useIsAdmin();

  const [health, setHealth] = useState<HealthSummary | null>(null);
  const [healthErr, setHealthErr] = useState<string | null>(null);
  const [stats, setStats] = useState<UserStat[] | null>(null);
  const [statsErr, setStatsErr] = useState<string | null>(null);
  // Manual refresh in flight (the header button's spinner). The 10s poll
  // refreshes silently — no spinner churn.
  const [refreshing, setRefreshing] = useState(false);

  useEffect(() => {
    if (admin === "member") router.replace("/settings");
  }, [admin, router]);

  // Apply a health fetch to state. Every setState happens inside the promise
  // callbacks — never synchronously in the effect body; `isStale` drops a
  // response that resolves after the component unmounts.
  const applyHealth = (isStale: () => boolean) =>
    fetch("/api/admin/health-summary", { cache: "no-store" })
      .then(async (res) => {
        if (!res.ok) throw new Error(`health request failed: ${res.status}`);
        return (await res.json()) as HealthSummary;
      })
      .then((d) => {
        if (isStale()) return;
        setHealth(d);
        setHealthErr(null);
      })
      .catch((e: unknown) => {
        if (isStale()) return;
        setHealthErr(e instanceof Error ? e.message : "failed to load health");
      });

  const applyStats = (isStale: () => boolean) =>
    fetchStats()
      .then((rows) => {
        if (isStale() || rows === null) return;
        setStats(rows);
        setStatsErr(null);
      })
      .catch((err: unknown) => {
        if (isStale()) return;
        setStatsErr(err instanceof Error ? err.message : "Failed to load.");
      });

  useEffect(() => {
    if (admin !== "admin") return;
    let stale = false;
    const isStale = () => stale;
    void applyHealth(isStale);
    void applyStats(isStale);
    const id = setInterval(() => void applyHealth(isStale), REFRESH_MS);
    return () => {
      stale = true;
      clearInterval(id);
    };
  }, [admin]);

  if (admin !== "admin") return null;

  // Manual refresh: refetches BOTH health and usage stats (setState in an
  // event handler is fine; results land via the apply* promise callbacks).
  const refresh = () => {
    setRefreshing(true);
    void Promise.allSettled([applyHealth(() => false), applyStats(() => false)]).then(() =>
      setRefreshing(false),
    );
  };

  const totalSpend = stats?.reduce((acc, u) => acc + u.total_cost_usd, 0) ?? 0;
  const totalTurns = stats?.reduce((acc, u) => acc + u.total_turns, 0) ?? 0;
  const usageItems: AdminStat[] = [
    { title: "Users", value: String(stats?.length ?? 0) },
    { title: "Turns total", value: String(totalTurns) },
    { title: "Spend total", value: formatUSD(totalSpend) },
    {
      title: "Most recent",
      value: stats && stats.length > 0 ? formatAgo(stats[0].last_activity) : "—",
    },
  ];

  // The design's .admin-health-head: title + DB badge + poll note, with the
  // refresh control hugging the right edge (it lives here, not the topbar).
  const healthHead = (
    <div className="mb-[0.8rem] flex flex-wrap items-center gap-[0.55rem]">
      <h3 className="m-0 text-[0.98rem] font-semibold text-[var(--color-text-primary)]">
        System health
      </h3>
      {health ? (
        <ConnBadge variant={health.db.chat === "healthy" ? "success" : "warn"}>
          chat DB {health.db.chat}
        </ConnBadge>
      ) : null}
      <span className="text-[0.72rem] text-[var(--color-text-muted)]">
        auto-refreshes every 10s
      </span>
      <button
        type="button"
        aria-label="Refresh now"
        disabled={refreshing}
        onClick={refresh}
        className="ml-auto inline-flex size-8 items-center justify-center rounded-[var(--radius-md)] border border-[var(--color-border)] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-50"
      >
        <Icon name="refresh" className={`size-[0.95rem]${refreshing ? " animate-spin" : ""}`} />
      </button>
    </div>
  );

  return (
    <SetSection
      title="Overview"
      intro="Live operator view of this deployment — health, the MCP catalog, and usage."
    >
      <ConnGroup>
        {healthErr ? (
          <>
            {healthHead}
            <NoticeBanner tone="danger" data-testid="health-panel-error">
              Health unavailable: {healthErr}
            </NoticeBanner>
          </>
        ) : !health ? (
          <>
            {healthHead}
            <div
              className="text-[0.85rem] text-[var(--color-text-muted)]"
              data-testid="health-panel-loading"
            >
              Loading system health…
            </div>
          </>
        ) : (
          <div data-testid="health-panel">
            {healthHead}
            <AdminStats items={healthItems(health)} />
            {health.mcp_servers.length > 0 ? (
              <div className="mt-[0.85rem] flex flex-wrap items-center gap-[0.4rem]">
                <span
                  className="mr-[0.25rem] text-[0.62rem] font-bold uppercase tracking-[0.1em] text-[var(--color-text-muted)]"
                  title="Optional-MCP catalog. Servers are not health-probed here; each chip's tooltip says whether it is on by default for new conversations."
                >
                  MCP catalog
                </span>
                {health.mcp_servers.map((s) => (
                  // NOT a health signal — the endpoint doesn't ping servers
                  // (see internal/httpapi/health.go). `enabled` means "on by
                  // default for new conversations"; the chips render uniformly
                  // (the design's .mcp-chip) and the per-chip tooltip carries
                  // the enabled/optional distinction — never a danger tint,
                  // which would wrongly read as broken.
                  <span
                    key={s.name}
                    title={
                      s.enabled
                        ? "On by default for new conversations"
                        : "Optional — off by default (users enable it per conversation)"
                    }
                    className="rounded-[var(--radius-pill)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-[0.6rem] py-[0.2rem] font-[family-name:var(--font-code)] text-[0.7rem] text-[var(--color-text-secondary)]"
                  >
                    {s.name}
                  </span>
                ))}
              </div>
            ) : null}
          </div>
        )}
      </ConnGroup>

      <ConnGroup>
        <ConnGroupHead title="Usage" />
        {statsErr ? (
          <NoticeBanner tone="danger">{statsErr}</NoticeBanner>
        ) : (
          <AdminStats items={usageItems} />
        )}
      </ConnGroup>
    </SetSection>
  );
}
