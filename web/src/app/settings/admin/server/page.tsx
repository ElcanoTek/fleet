"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";

import { useIsAdmin } from "../../useIsAdmin";
import { AdminStats, ConnGroup, SetSection, type AdminStat } from "../../ui/panels";
import { StoragePanel } from "./StoragePanel";
import { ConnBadge } from "../../ui/atoms";
import { NoticeBanner } from "@/app/shared/ui/NoticeBanner";
import { Icon } from "@/app/shared/ui/Icon";

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
  warnings?: string[];
};

const REFRESH_MS = 10_000;

function formatBytes(bytes: number, rate = false): string {
  if (!Number.isFinite(bytes) || bytes < 0) return "—";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  const digits = value >= 100 || unit === 0 ? 0 : value >= 10 ? 1 : 2;
  return `${value.toFixed(digits)} ${units[unit]}${rate ? "/s" : ""}`;
}

function formatUptime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "—";
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  return `${minutes}m`;
}

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

export default function AdminServerPage() {
  const router = useRouter();
  const admin = useIsAdmin();
  const [stats, setStats] = useState<ServerStats | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [refreshing, setRefreshing] = useState(false);

  useEffect(() => {
    if (admin === "member") router.replace("/settings");
  }, [admin, router]);

  const load = (isStale: () => boolean) =>
    fetch("/api/admin/server-stats", { cache: "no-store" })
      .then(async (res) => {
        if (!res.ok) throw new Error(`server stats request failed: ${res.status}`);
        return (await res.json()) as ServerStats;
      })
      .then((next) => {
        if (isStale()) return;
        setStats(next);
        setError(null);
      })
      .catch((err: unknown) => {
        if (!isStale()) setError(err instanceof Error ? err.message : "server stats unavailable");
      });

  useEffect(() => {
    if (admin !== "admin") return;
    let stale = false;
    void load(() => stale);
    const id = window.setInterval(() => void load(() => stale), REFRESH_MS);
    return () => {
      stale = true;
      window.clearInterval(id);
    };
  }, [admin]);

  if (admin !== "admin") return null;

  const refresh = () => {
    setRefreshing(true);
    void load(() => false).finally(() => setRefreshing(false));
  };

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
    <SetSection
      title="Server"
      intro="A lightweight view of this Fleet host. Use full observability tooling for alerts, history, and multi-node deployments."
    >
      <ConnGroup>
        <div className="mb-3 flex flex-wrap items-center gap-2">
          <h3 className="m-0 text-[0.98rem] font-semibold text-[var(--color-text-primary)]">Host resources</h3>
          {stats ? <ConnBadge variant={stats.available ? "success" : "warn"}>{stats.available ? "reporting" : "limited"}</ConnBadge> : null}
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

        {error ? (
          <NoticeBanner tone="danger" data-testid="server-stats-error">Server statistics unavailable: {error}</NoticeBanner>
        ) : !stats ? (
          <p className="text-sm text-[var(--color-text-muted)]" data-testid="server-stats-loading">Loading server statistics…</p>
        ) : (
          <div data-testid="server-stats-panel">
            <AdminStats items={cpuItems} />
            <div className="mt-5 border-t border-[var(--color-border)] pt-4">
              <AdminStats items={resourceItems} />
              {stats.memory.available ? <UsageBar label="Memory" used={stats.memory.used_bytes} total={stats.memory.total_bytes} /> : null}
              {stats.disk.available ? <UsageBar label="Root disk" used={stats.disk.used_bytes} total={stats.disk.total_bytes} /> : null}
            </div>
            {stats.warnings && stats.warnings.length > 0 ? (
              <NoticeBanner tone="warning" data-testid="server-stats-warnings">
                Some host metrics are unavailable: {stats.warnings.join("; ")}.
              </NoticeBanner>
            ) : null}
          </div>
        )}
      </ConnGroup>
      <StoragePanel />
    </SetSection>
  );
}
