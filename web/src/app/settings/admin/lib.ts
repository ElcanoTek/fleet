// Shared data helpers for the /settings/admin pages (fleet-unified settings
// pass). The per-user usage stats feed two surfaces — the Overview page's
// Usage cards and the Users page's table join — so the fetch (with its
// 401→/login redirect and 403 allowlist message) and the tiny formatters live
// here once, ported verbatim from the old /admin page.

export type UserStat = {
  email: string;
  conversation_count: number;
  pinned_count: number;
  last_activity: number;
  total_cost_usd: number;
  total_turns: number;
  total_prompt_tokens?: number;
  total_cached_tokens?: number;
  total_cache_creation_tokens?: number;
  uncached_prompt_tokens?: number;
  cache_hit_rate_pct?: number;
  // Struggle counts (docs/CHAT-ESCALATION.md). Optional so a payload from an
  // older server still parses; the table shows "—" when absent.
  frustrated_messages?: number;
  struggle_signals?: number;
  advanced_switches?: number;
};

export type DailyCacheStat = {
  day: string;
  turns: number;
  cost_usd: number;
  prompt_tokens: number;
  cached_tokens: number;
  uncached_prompt_tokens: number;
  cache_hit_rate_pct: number;
};

export type StatsResponse = {
  users: UserStat[];
  daily_cache?: DailyCacheStat[];
};

const compactFmt = new Intl.NumberFormat("en-US", {
  notation: "compact",
  maximumFractionDigits: 1,
});

export function formatCompactTokens(v: number | undefined): string {
  if (!v) return "0";
  return compactFmt.format(v);
}

export function formatAgo(unixSeconds: number): string {
  if (!unixSeconds) return "—";
  const seconds = Math.max(0, Math.floor(Date.now() / 1000) - unixSeconds);
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86400)}d ago`;
}

export function formatUSD(v: number): string {
  if (v == null) return "$0.00";
  return `$${v.toFixed(2)}`;
}

// formatBytes — the admin pages' byte formatter (Server health, Storage):
// binary units up to TB with adaptive precision (whole numbers from 100 up,
// one decimal from 10, two below), "—" for a missing or negative value.
// `rate` appends "/s" for the network throughput cards.
export function formatBytes(bytes: number, rate = false): string {
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

// formatUptime — "3d 4h", "4h 12m" or "12m"; "—" when unknown.
export function formatUptime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "—";
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  return `${minutes}m`;
}

// Pure data fetch — no React state — so callers (mount effects and refresh
// handlers) can apply the result inside their own promise callbacks. Returns
// the rows on success, or null when the request triggered a redirect
// (401 → /login) and there's nothing left to render. Throws on any other
// non-OK response so callers can surface it.
export async function fetchAdminStatsPayload(): Promise<StatsResponse | null> {
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
  return {
    users: data.users ?? [],
    daily_cache: data.daily_cache ?? [],
  };
}

export async function fetchStats(): Promise<UserStat[] | null> {
  const data = await fetchAdminStatsPayload();
  return data ? data.users : null;
}
