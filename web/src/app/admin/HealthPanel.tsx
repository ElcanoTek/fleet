"use client";

import { useEffect, useState } from "react";

// Built-in Fleet health dashboard (#301): a single-pane system-health panel that
// polls GET /api/admin/health-summary every 10s and renders runtime, DB, LLM
// spend, scheduler workers/tasks, sandbox pool, and MCP catalog. Deliberately
// dependency-free — number cards + status pills, no external chart library.

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

function Card({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <div className="rounded-[0.95rem] border border-[var(--color-border)] bg-[var(--gradient-surface-panel)] px-4 py-3">
      <div className="text-[0.7rem] uppercase tracking-wide text-[var(--color-text-muted)]">{label}</div>
      <div className="mt-1 text-[1.15rem] font-semibold text-[var(--color-text-primary)]">{value}</div>
      {hint ? <div className="mt-0.5 text-[0.7rem] text-[var(--color-text-muted)]">{hint}</div> : null}
    </div>
  );
}

function StatusPill({ ok, label }: { ok: boolean; label: string }) {
  const tone = ok
    ? "border-[var(--color-success-border)] bg-[color-mix(in_srgb,var(--color-success)_15%,transparent)] text-[var(--color-success)]"
    : "border-[var(--color-danger-border)] bg-[color-mix(in_srgb,var(--color-danger)_15%,transparent)] text-[var(--color-danger)]";
  return <span className={`rounded-full border px-2 py-0.5 text-[0.7rem] font-medium ${tone}`}>{label}</span>;
}

export function HealthPanel() {
  const [data, setData] = useState<HealthSummary | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let stale = false;
    const load = () => {
      fetch("/api/admin/health-summary", { cache: "no-store" })
        .then(async (res) => {
          if (!res.ok) throw new Error(`health request failed: ${res.status}`);
          return (await res.json()) as HealthSummary;
        })
        .then((d) => {
          if (stale) return;
          setData(d);
          setErr(null);
        })
        .catch((e: unknown) => {
          if (stale) return;
          setErr(e instanceof Error ? e.message : "failed to load health");
        });
    };
    load();
    const id = setInterval(load, REFRESH_MS);
    return () => {
      stale = true;
      clearInterval(id);
    };
  }, []);

  if (err) {
    return (
      <div
        data-testid="health-panel-error"
        className="mb-4 rounded-[0.95rem] border border-[#e08080] bg-[color-mix(in_srgb,#e08080_15%,transparent)] px-4 py-3 text-[0.85rem] text-[#e08080]"
      >
        Health unavailable: {err}
      </div>
    );
  }
  if (!data) {
    return (
      <div className="mb-4 text-[0.85rem] text-[var(--color-text-muted)]" data-testid="health-panel-loading">
        Loading system health…
      </div>
    );
  }

  return (
    <section data-testid="health-panel" className="mb-6">
      <div className="mb-2 flex items-center gap-2">
        <h2 className="text-[0.95rem] font-semibold text-[var(--color-text-primary)]">System health</h2>
        <StatusPill ok={data.db.chat === "healthy"} label={`chat DB ${data.db.chat}`} />
        <span className="text-[0.7rem] text-[var(--color-text-muted)]">auto-refreshes every 10s</span>
      </div>
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <Card label="Version" value={data.fleet_version} hint={`up ${formatUptime(data.uptime_seconds)}`} />
        <Card
          label="DB pool"
          value={`${data.db.in_use}/${data.db.pool_size}`}
          hint={`${data.db.idle} idle`}
        />
        <Card
          label="LLM today"
          value={`$${data.llm.cost_today_usd.toFixed(2)}`}
          hint={`${data.llm.calls_today} calls · $${data.llm.avg_cost_per_call.toFixed(3)}/call`}
        />
        <Card label="Active chats" value={String(data.conversations_active)} />
        {data.workers ? (
          <>
            {/* Single-box deploy: there are no separate worker nodes, so this is
                a task-throughput card (what the scheduler is doing right now),
                not a node count. */}
            <Card
              label="Tasks"
              value={`${data.workers.running_tasks} running`}
              hint={`${data.workers.queued_tasks} queued`}
            />
            <Card
              label="Tasks today"
              value={`${data.workers.completed_today} ✓`}
              hint={`${data.workers.failed_today} failed`}
            />
          </>
        ) : (
          <Card label="Tasks" value="—" hint="scheduler stats unavailable" />
        )}
        <Card
          label="Sandbox pool"
          value={data.sandbox_pool ? `${data.sandbox_pool.available}/${data.sandbox_pool.size}` : "—"}
          hint={data.sandbox_pool ? "ready / size" : "not configured"}
        />
        <Card
          label="Runtime"
          value={`${data.memory_mb} MB`}
          hint={`${data.goroutines} goroutines`}
        />
      </div>
      {data.mcp_servers.length > 0 ? (
        <div className="mt-3 flex flex-wrap items-center gap-2">
          <span className="text-[0.7rem] uppercase tracking-wide text-[var(--color-text-muted)]">MCP</span>
          {data.mcp_servers.map((s) => (
            <StatusPill key={s.name} ok={s.enabled} label={s.name} />
          ))}
        </div>
      ) : null}
    </section>
  );
}
