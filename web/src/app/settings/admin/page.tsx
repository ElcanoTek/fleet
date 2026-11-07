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

import { useEffect, useRef, useState } from "react";
import { useRouter } from "@/app/lib/navigation";

import {
  fetchAdminStatsPayload,
  formatAgo,
  formatCompactTokens,
  formatUSD,
  formatUptime,
  type DailyCacheStat,
  type UserStat,
} from "./lib";
import { ConnBadge } from "../ui/atoms";
import { AdminStats, ConnGroup, ConnGroupHead, SetSection, type AdminStat } from "../ui/panels";
import { useIsAdmin } from "../useIsAdmin";
import { AdminGateFallback } from "../AdminGateFallback";
import { Icon } from "@/app/shared/ui/Icon";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import { errorMessage } from "@/app/shared/lib/errorMessage";
import { useVisibleInterval } from "@/app/shared/hooks/useVisibleInterval";

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
  disk: {
    available: boolean;
    path: string;
    total_bytes: number;
    free_bytes: number;
    free_percent: number;
    min_free_percent: number;
    shedding: boolean;
    error?: string;
  } | null;
  memory_mb: number;
  goroutines: number;
};

const REFRESH_MS = 10_000;

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
  // Disk. `shedding` is the one state worth spelling out rather than showing a
  // number for: the box has already stopped claiming scheduled tasks, and
  // because chat keeps working, nothing else on this page would say so.
  if (d.disk) {
    items.push({
      title: "Disk",
      value: d.disk.available ? `${d.disk.free_percent.toFixed(1)}% free` : "—",
      sub: !d.disk.available
        ? (d.disk.error ?? "not measurable")
        : d.disk.shedding
          ? `below the ${d.disk.min_free_percent}% floor — scheduled tasks paused`
          : `${d.disk.path} · floor ${d.disk.min_free_percent}%`,
    });
  }
  return items;
}

export default function AdminOverviewPage() {
  const router = useRouter();
  const admin = useIsAdmin();

  const [health, setHealth] = useState<HealthSummary | null>(null);
  const [healthErr, setHealthErr] = useState<string | null>(null);
  const [stats, setStats] = useState<UserStat[] | null>(null);
  const [dailyCache, setDailyCache] = useState<DailyCacheStat[]>([]);
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
        setHealthErr(errorMessage(e, "failed to load health"));
      });

  const applyStats = (isStale: () => boolean) =>
    fetchAdminStatsPayload()
      .then((payload) => {
        if (isStale() || payload === null) return;
        setStats(payload.users);
        setDailyCache(payload.daily_cache ?? []);
        setStatsErr(null);
      })
      .catch((err: unknown) => {
        if (isStale()) return;
        setStatsErr(errorMessage(err, "Failed to load."));
      });

  // The poll's staleness check: the current load effect's, so a poll that
  // resolves after an unmount is dropped like the first load.
  const pollStale = useRef<() => boolean>(() => true);
  useEffect(() => {
    if (admin !== "admin") return;
    let stale = false;
    const isStale = () => stale;
    pollStale.current = isStale;
    void applyHealth(isStale);
    void applyStats(isStale);
    return () => {
      stale = true;
    };
  }, [admin]);
  // Health re-polls silently, resting while the tab is hidden (#2004).
  useVisibleInterval(() => void applyHealth(pollStale.current), REFRESH_MS, admin === "admin");

  if (admin !== "admin") return <AdminGateFallback state={admin} />;

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
  const totalPromptTok = stats?.reduce((acc, u) => acc + (u.total_prompt_tokens ?? 0), 0) ?? 0;
  const totalCachedTok = stats?.reduce((acc, u) => acc + (u.total_cached_tokens ?? 0), 0) ?? 0;
  const totalUncachedTok = Math.max(0, totalPromptTok - totalCachedTok);
  const overallHitPct = totalPromptTok > 0 ? (totalCachedTok / totalPromptTok) * 100 : 0;
  const usageItems: AdminStat[] = [
    { title: "Users", value: String(stats?.length ?? 0) },
    { title: "Turns total", value: String(totalTurns) },
    // All-time chat-side spend (GET /api/admin/stats sums turn_metrics only);
    // labeled as such so it can't be misread as the Operations Center Usage
    // tab's windowed chat+task total.
    { title: "Chat spend (all-time)", value: formatUSD(totalSpend) },
    {
      title: "Cache hit rate",
      value: totalPromptTok > 0 ? `${overallHitPct.toFixed(1)}%` : "—",
      sub:
        totalPromptTok > 0
          ? `${formatCompactTokens(totalCachedTok)} hit · ${formatCompactTokens(totalUncachedTok)} miss`
          : "no prompt tokens yet",
    },
    {
      title: "Most recent",
      value: stats && stats.length > 0 ? formatAgo(stats[0].last_activity) : "—",
    },
  ];
  const usersWithCache = (stats ?? []).filter(
    (u) => (u.total_prompt_tokens ?? 0) > 0 || (u.total_cached_tokens ?? 0) > 0,
  );

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

      {!statsErr && (dailyCache.length > 0 || usersWithCache.length > 0) ? (
        <ConnGroup>
          <div data-testid="cache-efficiency-panel" className="grid gap-[0.85rem]">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <h3 className="m-0 text-[0.98rem] font-semibold text-[var(--color-text-primary)]">
                Prompt cache efficiency
              </h3>
              <span className="font-[family-name:var(--font-code)] text-[0.7rem] text-[var(--color-text-muted)]">
                CLI: fleet cache-stats
              </span>
            </div>

            {dailyCache.length > 0 ? (
              <div className="overflow-x-auto">
                <div className="mb-1.5 text-[0.68rem] font-bold uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
                  By date (last 14 days, UTC)
                </div>
                <table className="w-full border-collapse text-left text-[0.78rem]">
                  <thead>
                    <tr className="border-b border-[var(--color-border)] text-[0.68rem] uppercase tracking-[0.06em] text-[var(--color-text-muted)]">
                      <th className="py-1.5 pr-3">Date</th>
                      <th className="py-1.5 px-2 text-right">Turns</th>
                      <th className="py-1.5 px-2 text-right">Prompt tok</th>
                      <th className="py-1.5 px-2 text-right">Cached</th>
                      <th className="py-1.5 px-2 text-right">Cache miss</th>
                      <th className="py-1.5 pl-2 text-right">Hit %</th>
                    </tr>
                  </thead>
                  <tbody>
                    {[...dailyCache].reverse().map((d) => (
                      <tr
                        key={d.day}
                        data-testid={`cache-day-${d.day}`}
                        className="border-b border-[var(--color-border-subtle)] text-[var(--color-text-secondary)]"
                      >
                        <td className="py-1.5 pr-3 font-[family-name:var(--font-code)] text-[var(--color-text-primary)]">
                          {d.day}
                        </td>
                        <td className="py-1.5 px-2 text-right tabular-nums">{d.turns}</td>
                        <td className="py-1.5 px-2 text-right tabular-nums">
                          {formatCompactTokens(d.prompt_tokens)}
                        </td>
                        <td className="py-1.5 px-2 text-right tabular-nums">
                          {formatCompactTokens(d.cached_tokens)}
                        </td>
                        <td className="py-1.5 px-2 text-right tabular-nums">
                          {formatCompactTokens(d.uncached_prompt_tokens)}
                        </td>
                        <td className="py-1.5 pl-2 text-right font-medium tabular-nums text-[var(--color-text-primary)]">
                          {d.cache_hit_rate_pct.toFixed(1)}%
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            ) : null}

            {usersWithCache.length > 0 ? (
              <div className="overflow-x-auto">
                <div className="mb-1.5 text-[0.68rem] font-bold uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
                  By user (all-time average)
                </div>
                <table className="w-full border-collapse text-left text-[0.78rem]">
                  <thead>
                    <tr className="border-b border-[var(--color-border)] text-[0.68rem] uppercase tracking-[0.06em] text-[var(--color-text-muted)]">
                      <th className="py-1.5 pr-3">User</th>
                      <th className="py-1.5 px-2 text-right">Turns</th>
                      <th className="py-1.5 px-2 text-right">Prompt tok</th>
                      <th className="py-1.5 px-2 text-right">Cached</th>
                      <th className="py-1.5 px-2 text-right">Cache miss</th>
                      <th className="py-1.5 pl-2 text-right">Hit %</th>
                    </tr>
                  </thead>
                  <tbody>
                    {usersWithCache.map((u) => {
                      const promptTok = u.total_prompt_tokens ?? 0;
                      const cachedTok = u.total_cached_tokens ?? 0;
                      const uncachedTok =
                        u.uncached_prompt_tokens ?? Math.max(0, promptTok - cachedTok);
                      const hitPct =
                        u.cache_hit_rate_pct ??
                        (promptTok > 0 ? (cachedTok / promptTok) * 100 : 0);
                      return (
                        <tr
                          key={u.email}
                          data-testid={`cache-user-${u.email}`}
                          className="border-b border-[var(--color-border-subtle)] text-[var(--color-text-secondary)]"
                        >
                          <td className="py-1.5 pr-3 text-[var(--color-text-primary)]">
                            {u.email}
                          </td>
                          <td className="py-1.5 px-2 text-right tabular-nums">{u.total_turns}</td>
                          <td className="py-1.5 px-2 text-right tabular-nums">
                            {formatCompactTokens(promptTok)}
                          </td>
                          <td className="py-1.5 px-2 text-right tabular-nums">
                            {formatCompactTokens(cachedTok)}
                          </td>
                          <td className="py-1.5 px-2 text-right tabular-nums">
                            {formatCompactTokens(uncachedTok)}
                          </td>
                          <td className="py-1.5 pl-2 text-right font-medium tabular-nums text-[var(--color-text-primary)]">
                            {hitPct.toFixed(1)}%
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            ) : null}
          </div>
        </ConnGroup>
      ) : null}
    </SetSection>
  );
}
