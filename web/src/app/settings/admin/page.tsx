import { formatFixed } from "@/app/lib/i18n";
import { t as tr } from "@/app/lib/i18n";
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
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
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
    { title: tr("Version"), value: d.fleet_version, sub: tr("up {0}", [formatUptime(d.uptime_seconds)]) },
    { title: tr("DB pool"), value: `${d.db.in_use}/${d.db.pool_size}`, sub: tr("{0} idle", [d.db.idle]) },
    {
      title: tr("LLM today"),
      value: `$${formatFixed(d.llm.cost_today_usd, 2)}`,
      sub: tr("{0} calls · ${1}/call", [d.llm.calls_today, formatFixed(d.llm.avg_cost_per_call, 3)]),
    },
    { title: tr("Active chats"), value: String(d.conversations_active) },
  ];
  if (d.workers) {
    // Single-box deploy: there are no separate worker nodes, so these are
    // task-throughput cards (what the scheduler is doing right now), not a
    // node count.
    items.push(
      {
        title: tr("Tasks"),
        value: `${d.workers.running_tasks} running`,
        sub: tr("{0} queued", [d.workers.queued_tasks]),
      },
      {
        title: tr("Tasks today"),
        value: `${d.workers.completed_today} ✓`,
        sub: tr("{0} failed", [d.workers.failed_today]),
      },
    );
  } else {
    items.push({ title: tr("Tasks"), value: "—", sub: tr("scheduler stats unavailable") });
  }
  items.push(
    {
      title: tr("Sandbox pool"),
      value: d.sandbox_pool ? `${d.sandbox_pool.available}/${d.sandbox_pool.size}` : "—",
      sub: d.sandbox_pool ? tr("ready / size") : tr("not configured"),
    },
    { title: tr("Runtime"), value: `${d.memory_mb} MB`, sub: tr("{0} goroutines", [d.goroutines]) },
  );
  // Disk. `shedding` is the one state worth spelling out rather than showing a
  // number for: the box has already stopped claiming scheduled tasks, and
  // because chat keeps working, nothing else on this page would say so.
  if (d.disk) {
    items.push({
      title: tr("Disk"),
      value: d.disk.available ? tr("{0}% free", [formatFixed(d.disk.free_percent, 1)]) : "—",
      sub: !d.disk.available
        ? (d.disk.error ?? tr("not measurable"))
        : d.disk.shedding
          ? tr("below the {0}% floor — scheduled tasks paused", [d.disk.min_free_percent])
          : tr("{0} · floor {1}%", [d.disk.path, d.disk.min_free_percent]),
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
        if (!res.ok) throw new Error(tr("health request failed: {0}", [res.status]));
        return (await res.json()) as HealthSummary;
      })
      .then((d) => {
        if (isStale()) return;
        setHealth(d);
        setHealthErr(null);
      })
      .catch((e: unknown) => {
        if (isStale()) return;
        setHealthErr(errorMessage(e, tr("failed to load health")));
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
        setStatsErr(errorMessage(err, tr("Failed to load.")));
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
    { title: tr("Users"), value: String(stats?.length ?? 0) },
    { title: tr("Turns total"), value: String(totalTurns) },
    // All-time chat-side spend (GET /api/admin/stats sums turn_metrics only);
    // labeled as such so it can't be misread as the Operations Center Usage
    // tab's windowed chat+task total.
    { title: tr("Chat spend (all-time)"), value: formatUSD(totalSpend) },
    {
      title: tr("Cache hit rate"),
      value: totalPromptTok > 0 ? `${formatFixed(overallHitPct, 1)}%` : "—",
      sub:
        totalPromptTok > 0
          ? tr("{0} hit · {1} miss", [formatCompactTokens(totalCachedTok), formatCompactTokens(totalUncachedTok)])
          : tr("no prompt tokens yet"),
    },
    {
      title: tr("Most recent"),
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
        {tr("System health")}
      </h3>
      {health ? (
        <ConnBadge variant={health.db.chat === "healthy" ? "success" : "warn"}>
          {tr("chat DB")}{" "}{tr(health.db.chat)}
        </ConnBadge>
      ) : null}
      <span className="text-[0.72rem] text-[var(--color-text-muted)]">
        {tr("auto-refreshes every 10s")}
      </span>
      <Button
        type="button"
        variant="ghost"
        size="icon-sm"
        aria-label={tr("Refresh now")}
        disabled={refreshing}
        onClick={refresh}
        className="ml-auto"
      >
        <Icon name="refresh" className={`size-[0.95rem]${refreshing ? " animate-spin" : ""}`} />
      </Button>
    </div>
  );

  return (
    <SetSection
      title={tr("Overview")}
      intro={tr("Live operator view of this deployment — health, the MCP catalog, and usage.")}
    >
      <ConnGroup>
        {healthErr ? (
          <>
            {healthHead}
            <NoticeBanner tone="danger" data-testid="health-panel-error">
              {tr("Health unavailable:")}{" "}{healthErr}
            </NoticeBanner>
          </>
        ) : !health ? (
          <>
            {healthHead}
            <div
              className="text-[0.85rem] text-[var(--color-text-muted)]"
              data-testid="health-panel-loading"
            >
              {tr("Loading system health…")}
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
                  title={tr("Optional-MCP catalog. Servers are not health-probed here; each chip's tooltip says whether it is on by default for new conversations.")}
                >
                  {tr("MCP catalog")}
                </span>
                {health.mcp_servers.map((s) => (
                  // NOT a health signal — the endpoint doesn't ping servers
                  // (see internal/httpapi/health.go). `enabled` means "on by
                  // default for new conversations"; the chips render uniformly
                  // (the design's .mcp-chip) and the per-chip tooltip carries
                  // the enabled/optional distinction — never a danger tint,
                  // which would wrongly read as broken.
                  <Badge
                    key={s.name}
                    variant="outline"
                    title={
                      s.enabled
                        ? tr("On by default for new conversations")
                        : tr("Optional — off by default (users enable it per conversation)")
                    }
                    className="bg-[var(--color-surface-1)] px-[0.6rem] py-[0.2rem] font-[family-name:var(--font-code)] text-[0.7rem] font-normal text-[var(--color-text-secondary)]"
                  >
                    {s.name}
                  </Badge>
                ))}
              </div>
            ) : null}
          </div>
        )}
      </ConnGroup>

      <ConnGroup>
        <ConnGroupHead title={tr("Usage")} />
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
                {tr("Prompt cache efficiency")}
              </h3>
              <span className="font-[family-name:var(--font-code)] text-[0.7rem] text-[var(--color-text-muted)]">
                {tr("CLI: fleet cache-stats")}
              </span>
            </div>

            {dailyCache.length > 0 ? (
              <div>
                <div className="mb-1.5 text-[0.68rem] font-bold uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
                  {tr("By date (last 14 days, UTC)")}
                </div>
                <Table className="text-[0.78rem]">
                  <TableHeader>
                    <TableRow className="text-[0.68rem] tracking-[0.06em]">
                      <TableHead className="h-8 py-1.5 pr-3">{tr("Date")}</TableHead>
                      <TableHead className="h-8 px-2 py-1.5 text-right">{tr("Turns")}</TableHead>
                      <TableHead className="h-8 px-2 py-1.5 text-right">{tr("Prompt tok")}</TableHead>
                      <TableHead className="h-8 px-2 py-1.5 text-right">{tr("Cached")}</TableHead>
                      <TableHead className="h-8 px-2 py-1.5 text-right">{tr("Cache miss")}</TableHead>
                      <TableHead className="h-8 py-1.5 pl-2 text-right">{tr("Hit %")}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {[...dailyCache].reverse().map((d) => (
                      <TableRow
                        key={d.day}
                        data-testid={`cache-day-${d.day}`}
                        className="border-[var(--color-border-subtle)] text-[var(--color-text-secondary)]"
                      >
                        <TableCell className="py-1.5 pr-3 font-[family-name:var(--font-code)] text-[var(--color-text-primary)]">
                          {d.day}
                        </TableCell>
                        <TableCell className="px-2 py-1.5 text-right tabular-nums">{d.turns}</TableCell>
                        <TableCell className="px-2 py-1.5 text-right tabular-nums">
                          {formatCompactTokens(d.prompt_tokens)}
                        </TableCell>
                        <TableCell className="px-2 py-1.5 text-right tabular-nums">
                          {formatCompactTokens(d.cached_tokens)}
                        </TableCell>
                        <TableCell className="px-2 py-1.5 text-right tabular-nums">
                          {formatCompactTokens(d.uncached_prompt_tokens)}
                        </TableCell>
                        <TableCell className="py-1.5 pl-2 text-right font-medium tabular-nums text-[var(--color-text-primary)]">
                          {formatFixed(d.cache_hit_rate_pct, 1)}%
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
            ) : null}

            {usersWithCache.length > 0 ? (
              <div>
                <div className="mb-1.5 text-[0.68rem] font-bold uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
                  {tr("By user (all-time average)")}
                </div>
                <Table className="text-[0.78rem]">
                  <TableHeader>
                    <TableRow className="text-[0.68rem] tracking-[0.06em]">
                      <TableHead className="h-8 py-1.5 pr-3">{tr("User")}</TableHead>
                      <TableHead className="h-8 px-2 py-1.5 text-right">{tr("Turns")}</TableHead>
                      <TableHead className="h-8 px-2 py-1.5 text-right">{tr("Prompt tok")}</TableHead>
                      <TableHead className="h-8 px-2 py-1.5 text-right">{tr("Cached")}</TableHead>
                      <TableHead className="h-8 px-2 py-1.5 text-right">{tr("Cache miss")}</TableHead>
                      <TableHead className="h-8 py-1.5 pl-2 text-right">{tr("Hit %")}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {usersWithCache.map((u) => {
                      const promptTok = u.total_prompt_tokens ?? 0;
                      const cachedTok = u.total_cached_tokens ?? 0;
                      const uncachedTok =
                        u.uncached_prompt_tokens ?? Math.max(0, promptTok - cachedTok);
                      const hitPct =
                        u.cache_hit_rate_pct ??
                        (promptTok > 0 ? (cachedTok / promptTok) * 100 : 0);
                      return (
                        <TableRow
                          key={u.email}
                          data-testid={`cache-user-${u.email}`}
                          className="border-[var(--color-border-subtle)] text-[var(--color-text-secondary)]"
                        >
                          <TableCell className="py-1.5 pr-3 text-[var(--color-text-primary)]">
                            {u.email}
                          </TableCell>
                          <TableCell className="px-2 py-1.5 text-right tabular-nums">{u.total_turns}</TableCell>
                          <TableCell className="px-2 py-1.5 text-right tabular-nums">
                            {formatCompactTokens(promptTok)}
                          </TableCell>
                          <TableCell className="px-2 py-1.5 text-right tabular-nums">
                            {formatCompactTokens(cachedTok)}
                          </TableCell>
                          <TableCell className="px-2 py-1.5 text-right tabular-nums">
                            {formatCompactTokens(uncachedTok)}
                          </TableCell>
                          <TableCell className="py-1.5 pl-2 text-right font-medium tabular-nums text-[var(--color-text-primary)]">
                            {formatFixed(hitPct, 1)}%
                          </TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              </div>
            ) : null}
          </div>
        </ConnGroup>
      ) : null}
    </SetSection>
  );
}
