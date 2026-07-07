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
};

type StatsResponse = {
  users: UserStat[];
};

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

// Pure data fetch — no React state — so callers (mount effects and refresh
// handlers) can apply the result inside their own promise callbacks. Returns
// the rows on success, or null when the request triggered a redirect
// (401 → /login) and there's nothing left to render. Throws on any other
// non-OK response so callers can surface it.
export async function fetchStats(): Promise<UserStat[] | null> {
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
