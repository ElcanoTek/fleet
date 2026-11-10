import { useEffect, useRef, useState } from "react";
import { useRouter } from "@/app/lib/navigation";

import { useIsAdmin } from "../../useIsAdmin";
import { AdminGateFallback } from "../../AdminGateFallback";
import { AdminStats, ConnGroup, SetSection, type AdminStat } from "../../ui/panels";
import { StoragePanel } from "./StoragePanel";
import { ConnBadge } from "../../ui/atoms";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import { Icon } from "@/app/shared/ui/Icon";
import { formatBytes, formatUptime } from "../lib";
import { errorMessage } from "@/app/shared/lib/errorMessage";
import { useVisibleInterval } from "@/app/shared/hooks/useVisibleInterval";

export type ServerAgentStats = {
  active_agents_total: number;
  active_chat_turns: number;
  running_tasks: number;
  queued_tasks: number;
  completed_tasks_today: number;
  failed_tasks_today: number;
  active_subagents: number;
  subagents_spawned_24h: number;
  subagents_spawned_total: number;
  tool_calls_24h: number;
  tool_calls_total: number;
  active_conversations_24h: number;
  llm_calls_today: number;
  llm_cost_today_usd: number;
  sandbox_pool_size: number;
  sandbox_pool_available: number;
  db_pool_in_use: number;
  db_pool_idle: number;
  db_pool_size: number;
  mcp_servers_enabled: number;
  mcp_servers_total: number;
  goroutines: number;
  process_memory_mb: number;
  fleet_version: string;
};

type ServerStats = {
  available: boolean;
  sampled_at: string;
  hostname?: string;
  platform: string;
  uptime_seconds: number;
  cpu: {
    available: boolean;
    cores: number;
    usage_percent: number | null;
    load_1: number;
    load_5: number;
    load_15: number;
  };
  memory: {
    available: boolean;
    total_bytes: number;
    used_bytes: number;
    available_bytes: number;
    swap_total_bytes: number;
    swap_used_bytes: number;
  };
  disk: {
    available: boolean;
    path: string;
    total_bytes: number;
    used_bytes: number;
    available_bytes: number;
    usage_percent: number;
  };
  network: {
    available: boolean;
    interfaces: number;
    received_bytes: number;
    transmitted_bytes: number;
    receive_bytes_per_second: number | null;
    transmit_bytes_per_second: number | null;
  };
  agents?: ServerAgentStats;
  warnings?: string[];
};

const REFRESH_MS = 10_000;
const plain = new Intl.NumberFormat("en-US");
const usd = new Intl.NumberFormat("en-US", {
  style: "currency",
  currency: "USD",
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
});

function percent(used: number, total: number): number {
  if (total <= 0) return 0;
  return Math.max(0, Math.min(100, (100 * used) / total));
}

function UsageBar({ label, used, total }: { label: string; used: number; total: number }) {
  const value = percent(used, total);
  return (
    <div className="mt-3" aria-label={`${label}: ${value.toFixed(1)}% used`}>
      <div className="mb-1.5 flex justify-between gap-3 text-xs text-[var(--color-text-muted)]">
        <span>{formatBytes(used)} used</span>
        <span>{formatBytes(total)} total</span>
      </div>
      <div className="h-2 overflow-hidden rounded-full bg-[var(--color-surface-2)]">
        <div
          className="h-full rounded-full bg-[var(--color-accent)] transition-[width]"
          style={{ width: `${value}%` }}
        />
      </div>
    </div>
  );
}

export function ServerStatsView({ allowCleanup = true }: { allowCleanup?: boolean } = {}) {
  const [stats, setStats] = useState<ServerStats | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [refreshing, setRefreshing] = useState(false);

  const load = (isStale: () => boolean) =>
    fetch("/api/admin/server-stats", { cache: "no-store" })
      .then(async (res) => {
        if (!res.ok) throw new Error(`server stats request failed: ${res.status}`);
        const parsed = (await res.json()) as ServerStats;
        if (!parsed || typeof parsed !== "object" || !parsed.cpu || !parsed.memory || !parsed.disk || !parsed.network) {
          throw new Error("server stats unavailable on this server");
        }
        return parsed;
      })
      .then((next) => {
        if (isStale()) return;
        setStats(next);
        setError(null);
      })
      .catch((err: unknown) => {
        if (!isStale()) setError(errorMessage(err, "server stats unavailable"));
      });

  // The poll's staleness check: the current load effect's, so a poll that
  // resolves after an unmount is dropped like the first load.
  const pollStale = useRef<() => boolean>(() => true);
  useEffect(() => {
    let stale = false;
    const isStale = () => stale;
    pollStale.current = isStale;
    void load(isStale);
    return () => {
      stale = true;
    };
  }, []);
  // Re-polls while the tab is visible, and once when it comes back (#2004).
  useVisibleInterval(() => void load(pollStale.current), REFRESH_MS, true);

  const refresh = () => {
    setRefreshing(true);
    void load(() => false).finally(() => setRefreshing(false));
  };

  const ag: ServerAgentStats = stats?.agents ?? {
    active_agents_total: 0,
    active_chat_turns: 0,
    running_tasks: 0,
    queued_tasks: 0,
    completed_tasks_today: 0,
    failed_tasks_today: 0,
    active_subagents: 0,
    subagents_spawned_24h: 0,
    subagents_spawned_total: 0,
    tool_calls_24h: 0,
    tool_calls_total: 0,
    active_conversations_24h: 0,
    llm_calls_today: 0,
    llm_cost_today_usd: 0,
    sandbox_pool_size: 0,
    sandbox_pool_available: 0,
    db_pool_in_use: 0,
    db_pool_idle: 0,
    db_pool_size: 0,
    mcp_servers_enabled: 0,
    mcp_servers_total: 0,
    goroutines: 0,
    process_memory_mb: 0,
    fleet_version: "",
  };

  const agentPrimaryItems: AdminStat[] = stats
    ? [
        {
          title: "Total active agents",
          value: plain.format(ag.active_agents_total),
          sub: `${ag.active_chat_turns} chat · ${ag.running_tasks} task bots · ${ag.active_subagents} sub-agents`,
        },
        {
          title: "Live chat agents",
          value: plain.format(ag.active_chat_turns),
          sub: `${plain.format(ag.active_conversations_24h)} active chats in 24h`,
        },
        {
          title: "Scheduled task bots",
          value: plain.format(ag.running_tasks),
          sub: `${ag.queued_tasks} queued · ${ag.completed_tasks_today} completed today · ${ag.failed_tasks_today} failed`,
        },
        {
          title: "Sub-agents used by bots",
          value: `${plain.format(ag.active_subagents)} active · ${plain.format(ag.subagents_spawned_24h)} (24h)`,
          sub: `${plain.format(ag.subagents_spawned_total)} total sub-agents spawned`,
        },
      ]
    : [];

  const agentRuntimeItems: AdminStat[] = stats
    ? [
        {
          title: "Tool executions (24h)",
          value: plain.format(ag.tool_calls_24h),
          sub: `${plain.format(ag.tool_calls_total)} total tool calls logged`,
        },
        {
          title: "LLM calls today",
          value: plain.format(ag.llm_calls_today),
          sub: `${usd.format(ag.llm_cost_today_usd)} runtime spend today (UTC)`,
        },
        {
          title: "Warm sandbox containers",
          value: `${ag.sandbox_pool_available} / ${ag.sandbox_pool_size}`,
          sub: "ready for isolated code & tool runs",
        },
        {
          title: "DB pool & runtime",
          value: `${ag.db_pool_in_use} / ${ag.db_pool_size} DB conns`,
          sub: `${ag.mcp_servers_enabled}/${ag.mcp_servers_total} MCP connectors · ${ag.goroutines} goroutines · ${ag.process_memory_mb.toFixed(1)} MB heap`,
        },
      ]
    : [];

  const cpuItems: AdminStat[] = stats
    ? [
        {
          title: "CPU",
          value: stats.cpu.usage_percent === null ? "Sampling…" : `${stats.cpu.usage_percent.toFixed(1)}%`,
          sub: `${stats.cpu.cores} logical cores`,
        },
        { title: "Load", value: stats.cpu.load_1.toFixed(2), sub: `${stats.cpu.load_5.toFixed(2)} / ${stats.cpu.load_15.toFixed(2)} (5m / 15m)` },
        { title: "Host uptime", value: formatUptime(stats.uptime_seconds) },
        { title: "Platform", value: stats.platform || "—", sub: stats.hostname || "hostname unavailable" },
      ]
    : [];

  const resourceItems: AdminStat[] = stats
    ? [
        {
          title: "Memory",
          value: stats.memory.available ? `${percent(stats.memory.used_bytes, stats.memory.total_bytes).toFixed(1)}%` : "—",
          sub: stats.memory.available
            ? `${formatBytes(stats.memory.available_bytes)} available${stats.memory.swap_total_bytes > 0 ? ` · ${formatBytes(stats.memory.swap_used_bytes)} swap used` : ""}`
            : "unavailable",
        },
        {
          title: "Root disk",
          value: stats.disk.available ? `${stats.disk.usage_percent.toFixed(1)}%` : "—",
          sub: stats.disk.available ? `${formatBytes(stats.disk.available_bytes)} available` : "unavailable",
        },
        {
          title: "Network in",
          value: stats.network.receive_bytes_per_second === null ? "Sampling…" : formatBytes(stats.network.receive_bytes_per_second, true),
          sub: `${formatBytes(stats.network.received_bytes)} since boot`,
        },
        {
          title: "Network out",
          value: stats.network.transmit_bytes_per_second === null ? "Sampling…" : formatBytes(stats.network.transmit_bytes_per_second, true),
          sub: `${formatBytes(stats.network.transmitted_bytes)} since boot`,
        },
      ]
    : [];

  return (
    <>
      <ConnGroup>
        <div className="mb-3 flex flex-wrap items-center gap-2">
          <h3 className="m-0 text-[0.98rem] font-semibold text-[var(--color-text-primary)]">
            Bot &amp; agent activity
          </h3>
          {stats ? (
            <ConnBadge variant={ag.active_agents_total > 0 ? "success" : "neutral"}>
              {ag.active_agents_total > 0
                ? `${ag.active_agents_total} active now`
                : "idle"}
            </ConnBadge>
          ) : null}
          <span className="text-xs text-[var(--color-text-muted)]">auto-refreshes every 10s</span>
          <button
            type="button"
            aria-label="Refresh server stats"
            disabled={refreshing}
            onClick={refresh}
            className="ml-auto inline-flex size-8 items-center justify-center rounded-[var(--radius-md)] border border-[var(--color-border)] text-[var(--color-text-secondary)] transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] disabled:opacity-50"
          >
            <Icon name="refresh" className={`size-[0.95rem]${refreshing ? " animate-spin" : ""}`} />
          </button>
        </div>

        {/* A failed poll keeps the last good sample on screen under the
            banner. Replacing the panel with the banner made every transient
            10s-poll failure flicker the whole page out and back. */}
        {error ? (
          <NoticeBanner tone="danger" data-testid="server-stats-error" className={stats ? "mb-3" : undefined}>
            Server statistics unavailable: {error}
            {stats ? " — showing the last successful sample." : ""}
          </NoticeBanner>
        ) : null}
        {!stats ? (
          error ? null : (
            <p className="text-sm text-[var(--color-text-muted)]" data-testid="server-stats-loading">
              Loading server statistics…
            </p>
          )
        ) : (
          <div data-testid="server-agent-stats-panel">
            <AdminStats items={agentPrimaryItems} />
            <div className="mt-4 border-t border-[var(--color-border)] pt-4">
              <AdminStats items={agentRuntimeItems} />
            </div>
          </div>
        )}
      </ConnGroup>

      {stats ? (
        <ConnGroup>
          <div className="mb-3 flex flex-wrap items-center gap-2">
            <h3 className="m-0 text-[0.98rem] font-semibold text-[var(--color-text-primary)]">Host resources</h3>
            <ConnBadge variant={stats.available ? "success" : "warn"}>
              {stats.available ? "reporting" : "limited"}
            </ConnBadge>
            {ag.fleet_version ? (
              <span className="font-[family-name:var(--font-code)] text-xs text-[var(--color-text-muted)]">
                {ag.fleet_version}
              </span>
            ) : null}
          </div>
          <div data-testid="server-stats-panel">
            <AdminStats items={cpuItems} />
            <div className="mt-5 border-t border-[var(--color-border)] pt-4">
              <AdminStats items={resourceItems} />
              {stats.memory.available ? (
                <UsageBar label="Memory" used={stats.memory.used_bytes} total={stats.memory.total_bytes} />
              ) : null}
              {stats.disk.available ? (
                <UsageBar label="Root disk" used={stats.disk.used_bytes} total={stats.disk.total_bytes} />
              ) : null}
            </div>
            {stats.warnings && stats.warnings.length > 0 ? (
              <NoticeBanner tone="warning" data-testid="server-stats-warnings">
                Some host metrics are unavailable: {stats.warnings.join("; ")}.
              </NoticeBanner>
            ) : null}
          </div>
        </ConnGroup>
      ) : null}

      <StoragePanel allowCleanup={allowCleanup} />
    </>
  );
}

export default function AdminServerPage() {
  const router = useRouter();
  const admin = useIsAdmin();

  useEffect(() => {
    if (admin === "member") router.replace("/settings");
  }, [admin, router]);

  if (admin !== "admin") return <AdminGateFallback state={admin} />;

  return (
    <SetSection
      title="Server"
      intro="Live bot and sub-agent concurrency, host resources, and workspace storage for this Fleet node."
    >
      <ServerStatsView allowCleanup={true} />
    </SetSection>
  );
}
