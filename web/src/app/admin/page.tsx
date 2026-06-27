"use client";

import Image from "next/image";
import Link from "next/link";
import { useEffect, useState } from "react";
import { HealthPanel } from "./HealthPanel";

// The admin page is intentionally minimalist — a single table keyed on
// user email. Power tools belong in real observability (Grafana, etc);
// this is a "who's active, who's costing money, when did I last see
// activity" sanity check for the operator of a 10-20 user box.

type UserStat = {
  email: string;
  conversation_count: number;
  pinned_count: number;
  last_activity: number;
  total_cost_usd: number;
  total_turns: number;
};

type StatsResponse = {
  users: UserStat[];
};

function formatAgo(unixSeconds: number): string {
  if (!unixSeconds) return "—";
  const seconds = Math.max(0, Math.floor(Date.now() / 1000) - unixSeconds);
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86400)}d ago`;
}

function formatUSD(v: number): string {
  if (v == null) return "$0.00";
  return `$${v.toFixed(2)}`;
}

// Pure data fetch — no React state — so callers (the mount effect and the
// refresh handler) can apply the result inside their own promise callbacks.
// Returns the rows on success, or null when the request triggered a redirect
// (401 → /login) and there's nothing left to render. Throws on any other
// non-OK response so callers can surface it.
async function fetchStats(): Promise<UserStat[] | null> {
  const response = await fetch("/api/admin/stats", { cache: "no-store" });
  if (response.status === 401) {
    window.location.href = "/login";
    return null;
  }
  if (response.status === 403) {
    throw new Error("You are not on the admin allowlist.");
  }
  if (!response.ok) {
    throw new Error(`Stats request failed: ${response.status}`);
  }
  const data = (await response.json()) as StatsResponse;
  return data.users ?? [];
}

function loadErrorMessage(err: unknown): string {
  return err instanceof Error ? err.message : "Failed to load.";
}

export default function AdminPage() {
  const [stats, setStats] = useState<UserStat[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  // Apply a fetchStats() result to state. Every setState happens inside the
  // promise callbacks — never synchronously in the effect body — so we don't
  // trip react-hooks/set-state-in-effect or kick off a cascading render.
  // `isStale` (wired up by the effect) drops a response that resolves after
  // the component unmounts.
  const applyStats = (isStale: () => boolean) => {
    fetchStats()
      .then((users) => {
        if (isStale() || users === null) return;
        setStats(users);
      })
      .catch((err: unknown) => {
        if (isStale()) return;
        setError(loadErrorMessage(err));
      })
      .finally(() => {
        if (isStale()) return;
        setLoading(false);
      });
  };

  // Manual refresh: setState in an event handler is fine; the fetched rows
  // land via applyStats's promise callbacks.
  const refresh = () => {
    setError(null);
    setLoading(true);
    applyStats(() => false);
  };

  useEffect(() => {
    let stale = false;
    applyStats(() => stale);
    return () => {
      stale = true;
    };
  }, []);

  const total = stats?.reduce((acc, u) => acc + u.total_cost_usd, 0) ?? 0;
  const totalTurns = stats?.reduce((acc, u) => acc + u.total_turns, 0) ?? 0;

  return (
    <main className="min-h-screen bg-[var(--gradient-bg-home-signature)] px-6 py-10 text-[var(--color-text-primary)]">
      <div className="mx-auto w-full max-w-4xl">
        <header className="mb-6 flex items-center justify-between gap-4">
          <Link href="/" className="flex items-center gap-2.5 no-underline">
            <Image src="/logos/elcano-mark-primary.svg" alt="Elcano" width={28} height={28} priority />
            <span className="font-heading text-[0.9375rem] font-semibold">Admin</span>
          </Link>
          <div className="flex items-center gap-2 text-[0.8125rem] text-[var(--color-text-secondary)]">
            <button
              type="button"
              onClick={refresh}
              className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
              disabled={loading}
            >
              {loading ? "Loading…" : "Refresh"}
            </button>
            <Link
              href="/"
              className="rounded-full border border-[var(--color-border-strong)] px-3 py-1 transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
            >
              Back to chat
            </Link>
          </div>
        </header>

        <HealthPanel />

        {error ? (
          <div className="rounded-[0.95rem] border border-[#e08080] bg-[color-mix(in_srgb,#e08080_15%,transparent)] px-4 py-3 text-[0.875rem] text-[#e08080]">
            {error}
          </div>
        ) : (
          <>
            <div className="mb-4 grid grid-cols-2 gap-3 sm:grid-cols-4">
              <Summary label="Users" value={String(stats?.length ?? 0)} />
              <Summary label="Turns total" value={String(totalTurns)} />
              <Summary label="Spend total" value={formatUSD(total)} />
              <Summary
                label="Most recent"
                value={
                  stats && stats.length > 0
                    ? formatAgo(stats[0].last_activity)
                    : "—"
                }
              />
            </div>

            <div className="overflow-hidden rounded-[1rem] border border-[var(--color-border)] bg-[var(--gradient-surface-panel)]">
              <table className="w-full text-[0.875rem]">
                <thead className="text-[0.75rem] uppercase tracking-wide text-[var(--color-text-muted)]">
                  <tr className="border-b border-[var(--color-border)]">
                    <th className="px-4 py-2 text-left">User</th>
                    <th className="px-4 py-2 text-right">Convs</th>
                    <th className="px-4 py-2 text-right">Pinned</th>
                    <th className="px-4 py-2 text-right">Turns</th>
                    <th className="px-4 py-2 text-right">Spend</th>
                    <th className="px-4 py-2 text-right">Last active</th>
                  </tr>
                </thead>
                <tbody>
                  {loading ? (
                    <tr>
                      <td colSpan={6} className="px-4 py-4 text-center text-[var(--color-text-muted)]">
                        Loading…
                      </td>
                    </tr>
                  ) : stats && stats.length > 0 ? (
                    stats.map((u) => (
                      <tr key={u.email} className="border-b border-[var(--color-border-subtle)] last:border-none">
                        <td className="px-4 py-2 text-[var(--color-text-primary)]">{u.email}</td>
                        <td className="px-4 py-2 text-right">{u.conversation_count}</td>
                        <td className="px-4 py-2 text-right">{u.pinned_count}</td>
                        <td className="px-4 py-2 text-right">{u.total_turns}</td>
                        <td className="px-4 py-2 text-right">{formatUSD(u.total_cost_usd)}</td>
                        <td className="px-4 py-2 text-right text-[var(--color-text-muted)]">
                          {formatAgo(u.last_activity)}
                        </td>
                      </tr>
                    ))
                  ) : (
                    <tr>
                      <td colSpan={6} className="px-4 py-4 text-center text-[var(--color-text-muted)]">
                        No user activity recorded yet.
                      </td>
                    </tr>
                  )}
                </tbody>
              </table>
            </div>
          </>
        )}
      </div>
    </main>
  );
}

function Summary({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-[0.85rem] border border-[var(--color-border)] bg-[var(--color-overlay-soft)] px-3 py-2">
      <p className="text-[0.6875rem] uppercase tracking-wide text-[var(--color-text-muted)]">{label}</p>
      <p className="mt-1 text-[1.1rem] font-semibold text-[var(--color-text-primary)]">{value}</p>
    </div>
  );
}
