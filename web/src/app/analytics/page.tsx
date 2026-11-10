import { signOutAfter } from "@/app/shared/signOut";
import { useCallback, useMemo, useState } from "react";
import {
  orchestratorApi,
  type UsageBucket,
} from "@/app/shared/lib/orchestratorApi";
import { useOrchestratorSession } from "@/app/shared/hooks/useOrchestratorSession";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";
import { useClientConfig } from "@/app/lib/useClientConfig";
import { Link } from "@/app/lib/Link";
import { NavRail, useRailCollapse } from "@/app/shared/ui/NavRail";
import { PageTopBar } from "@/app/shared/ui/PageTopBar";
import { ToastProvider } from "@/app/shared/ui/Toast";
import { UsagePanel } from "@/app/orchestrator/UsagePanel";
import { AdoptionPanel } from "@/app/orchestrator/AdoptionPanel";
import { ServerStatsView } from "@/app/settings/admin/server/page";
import { errorMessage } from "@/app/shared/lib/errorMessage";

const ANALYTICS_TABS = ["models", "outliers", "usage", "adoption", "server"] as const;
export type AnalyticsTab = (typeof ANALYTICS_TABS)[number];

function initialAnalyticsTab(): AnalyticsTab {
  if (typeof window === "undefined") return "models";
  const want = new URLSearchParams(window.location.search).get("tab");
  return want && (ANALYTICS_TABS as readonly string[]).includes(want)
    ? (want as AnalyticsTab)
    : "models";
}

type UserSignalRow = {
  email: string;
  conversation_count: number;
  pinned_count: number;
  last_activity: number;
  total_cost_usd: number;
  total_turns: number;
  total_prompt_tokens: number;
  total_cached_tokens: number;
  uncached_prompt_tokens: number;
  cache_hit_rate_pct: number;
  frustrated_messages: number;
  struggle_signals: number;
  advanced_switches: number;
};

type ConversationOutlierRow = {
  conversation_id: string;
  title: string;
  user_email: string;
  model: string;
  turns: number;
  prompt_tokens: number;
  completion_tokens: number;
  cached_tokens: number;
  cost_usd: number;
  frustrated_messages: number;
  struggle_signals: number;
  advanced_switches: number;
  updated_at: number;
};

type TaskOutlierRow = {
  task_id: string;
  title: string;
  owner_email: string;
  status: string;
  model: string;
  recurrence?: string;
  iterations: number;
  prompt_tokens: number;
  completion_tokens: number;
  cost_usd: number;
  error_message?: string;
};

type AdminStatsResponse = {
  users: UserSignalRow[];
  top_conversations?: ConversationOutlierRow[];
  top_tasks?: TaskOutlierRow[];
};

type AdminSettingItem = {
  key: string;
  value: string;
  default_value: string;
  source: "default" | "admin";
};

type AdminSettingsResponse = {
  settings: AdminSettingItem[];
};

const usd = new Intl.NumberFormat("en-US", {
  style: "currency",
  currency: "USD",
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
});
const usdFine = new Intl.NumberFormat("en-US", {
  style: "currency",
  currency: "USD",
  minimumFractionDigits: 2,
  maximumFractionDigits: 4,
});
const compact = new Intl.NumberFormat("en-US", {
  notation: "compact",
  maximumFractionDigits: 1,
});
const plain = new Intl.NumberFormat("en-US");

function fmtUSD(v: number): string {
  return v > 0 && v < 0.01 ? usdFine.format(v) : usd.format(v);
}

function fmtTokens(v: number): string {
  return compact.format(v);
}

function currentUTCMonthStart(): string {
  const now = new Date();
  const y = now.getUTCFullYear();
  const m = String(now.getUTCMonth() + 1).padStart(2, "0");
  return `${y}-${m}-01`;
}

type PinnedModelMeta = {
  slug: string;
  label?: string;
  free?: boolean;
  inputPricePerMtok?: number;
  outputPricePerMtok?: number;
};

function modelDisplayName(
  b: UsageBucket,
  pinned?: ReadonlyArray<PinnedModelMeta>,
): string {
  if (b.key === "") {
    return b.label
      ? `${b.label} (deleted chats & default-model turns)`
      : "(deleted chats & default-model turns)";
  }
  const cleanKey = b.key.trim().toLowerCase().replace(/^openrouter\//, "");
  const matchedPin = pinned?.find(
    (p) => p.slug.trim().toLowerCase().replace(/^openrouter\//, "") === cleanKey,
  );
  if (matchedPin?.label && matchedPin.label.trim() !== "") {
    return `${matchedPin.label.trim()} (${b.key})`;
  }
  return b.key;
}

function classifyModelBucket(
  b: UsageBucket,
  pinned: ReadonlyArray<PinnedModelMeta> | undefined,
): "included" | "billed" | "unpriced" {
  const rawKey = (b.key ?? "").trim().toLowerCase();
  const cleanKey = rawKey.replace(/^openrouter\//, "");
  const matchedPin =
    rawKey && pinned
      ? pinned.find(
          (p) =>
            p.slug.trim().toLowerCase().replace(/^openrouter\//, "") === cleanKey,
        )
      : undefined;
  const isPinnedZeroRated =
    !!matchedPin &&
    (matchedPin.free === true ||
      (matchedPin.inputPricePerMtok === 0 &&
        matchedPin.outputPricePerMtok === 0));
  if (rawKey && (cleanKey.endsWith(":free") || isPinnedZeroRated)) {
    return "included";
  }
  if (b.cost_usd > 0) return "billed";
  if (!rawKey) return "unpriced";
  if (
    matchedPin &&
    ((matchedPin.inputPricePerMtok ?? 0) > 0 ||
      (matchedPin.outputPricePerMtok ?? 0) > 0)
  ) {
    return "unpriced";
  }
  const hasFreePinned =
    !!pinned &&
    pinned.some(
      (p) =>
        p.free === true ||
        (p.inputPricePerMtok === 0 && p.outputPricePerMtok === 0),
    );
  if (hasFreePinned) {
    return rawKey.startsWith("openrouter/") ? "included" : "unpriced";
  }
  if (rawKey.startsWith("openrouter/")) {
    return "included";
  }
  return "unpriced";
}

function MonthlyAllowanceBanner() {
  const { models } = useClientConfig();
  const pinnedModels = models?.pinned;
  const monthStart = useMemo(() => currentUTCMonthStart(), []);
  const { data: monthUsage, loading: usageLoading } = useCancellableFetch(
    useCallback(
      (signal: AbortSignal) =>
        orchestratorApi.usage({ groupBy: "model", from: monthStart }, signal),
      [monthStart],
    ),
    [monthStart],
  );

  const [settingsNonce, setSettingsNonce] = useState(0);
  const [editingLimit, setEditingLimit] = useState(false);
  const [limitDraft, setLimitDraft] = useState("");
  const [savingLimit, setSavingLimit] = useState(false);
  const [limitError, setLimitError] = useState<string | null>(null);

  const { data: settingsData } = useCancellableFetch(
    useCallback(
      async (signal: AbortSignal): Promise<AdminSettingsResponse | null> => {
        try {
          const res = await fetch("/api/admin/settings", {
            credentials: "same-origin",
            signal,
          });
          if (!res.ok) return null;
          return (await res.json()) as AdminSettingsResponse;
        } catch {
          return null;
        }
      },
      [],
    ),
    [settingsNonce],
  );

  const limitItem = settingsData?.settings?.find(
    (s) => s.key === "monthly_soft_token_limit",
  );
  const parsedLimit = limitItem ? Number.parseInt(limitItem.value, 10) : 0;
  const softLimit =
    Number.isFinite(parsedLimit) && parsedLimit > 0 ? parsedLimit : 0;
  const limitSource: "default" | "admin" = limitItem?.source ?? "default";

  const saveSoftLimit = async (rawValue: string) => {
    setSavingLimit(true);
    setLimitError(null);
    try {
      const cleaned = rawValue.replace(/[,_\s]/g, "").trim();
      const res = await fetch("/api/admin/settings/monthly_soft_token_limit", {
        method: "PUT",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ value: cleaned || "0" }),
      });
      if (!res.ok) {
        const text = await res.text();
        throw new Error(text || `HTTP ${res.status}`);
      }
      setSettingsNonce((n) => n + 1);
      setEditingLimit(false);
    } catch (err) {
      setLimitError(errorMessage(err, "Failed to update monthly soft token limit"));
    } finally {
      setSavingLimit(false);
    }
  };

  const buckets = monthUsage?.buckets ?? [];
  const totalTokens =
    (monthUsage?.totals?.prompt_tokens ?? 0) +
    (monthUsage?.totals?.completion_tokens ?? 0);
  const totalCostUSD = monthUsage?.totals?.cost_usd ?? 0;
  const cacheHitPct = monthUsage?.totals?.cache_hit_rate_pct ?? 0;

  let includedTokens = 0;
  let billedTokens = 0;
  let includedCostUSD = 0;
  let includedChatCostUSD = 0;
  let includedTaskCostUSD = 0;
  for (const b of buckets) {
    const t = b.prompt_tokens + b.completion_tokens;
    const tier = classifyModelBucket(b, pinnedModels);
    if (tier === "included") {
      includedTokens += t;
      includedCostUSD += b.cost_usd;
      includedChatCostUSD += b.chat_cost_usd;
      includedTaskCostUSD += b.task_cost_usd;
    } else if (tier === "billed") {
      billedTokens += t;
    }
  }
  const billedCostUSD = Math.max(0, totalCostUSD - includedCostUSD);
  const billedChatCostUSD = Math.max(
    0,
    (monthUsage?.totals?.chat_cost_usd ?? 0) - includedChatCostUSD,
  );
  const billedTaskCostUSD = Math.max(
    0,
    (monthUsage?.totals?.task_cost_usd ?? 0) - includedTaskCostUSD,
  );
  const includedSharePct =
    totalTokens > 0 ? (includedTokens / totalTokens) * 100 : 0;
  const limitPct = softLimit > 0 ? (totalTokens / softLimit) * 100 : 0;

  return (
    <section
      className="mb-6 rounded-[var(--radius-lg)] border border-[var(--color-border)] bg-[var(--color-surface-1)] p-5 shadow-[var(--shadow-sm)]"
      aria-label="Monthly token allowance and spend overview"
      data-testid="analytics-monthly-banner"
    >
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <p className="text-[0.72rem] font-semibold uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
            Current UTC Month (since {monthStart})
          </p>
          <h2 className="mt-0.5 font-heading text-[1.1rem] font-bold text-[var(--color-text-primary)]">
            Workspace Token Allowance &amp; Model Mix Overview
          </h2>
        </div>
        <div className="flex items-center gap-2">
          {!editingLimit ? (
            <button
              type="button"
              className="btn btn-ghost text-[0.78rem]"
              data-testid="edit-monthly-soft-limit"
              onClick={() => {
                setLimitDraft(softLimit > 0 ? String(softLimit) : "");
                setLimitError(null);
                setEditingLimit(true);
              }}
            >
              {softLimit > 0 ? "Edit monthly token limit" : "Set monthly token limit"}
            </button>
          ) : (
            <form
              className="flex flex-wrap items-center gap-2"
              onSubmit={(e) => {
                e.preventDefault();
                void saveSoftLimit(limitDraft);
              }}
            >
              <label htmlFor="monthlySoftTokenLimitInput" className="sr-only">
                Monthly soft token limit
              </label>
              <input
                id="monthlySoftTokenLimitInput"
                type="text"
                inputMode="numeric"
                placeholder="e.g. 500000000 (0 = unlimited)"
                value={limitDraft}
                onChange={(e) => setLimitDraft(e.target.value)}
                className="w-full max-w-full rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-2)] px-2.5 py-1 font-[family-name:var(--font-code)] text-[0.8rem] text-[var(--color-text-primary)] sm:w-52"
                data-testid="monthly-soft-limit-input"
              />
              <button
                type="submit"
                disabled={savingLimit}
                className="btn text-[0.78rem]"
                data-testid="save-monthly-soft-limit"
              >
                {savingLimit ? "Saving…" : "Save"}
              </button>
              {softLimit > 0 ? (
                <button
                  type="button"
                  disabled={savingLimit}
                  className="btn btn-ghost text-[0.78rem]"
                  onClick={() => void saveSoftLimit("0")}
                >
                  Clear
                </button>
              ) : null}
              <button
                type="button"
                disabled={savingLimit}
                className="btn btn-ghost text-[0.78rem]"
                onClick={() => setEditingLimit(false)}
              >
                Cancel
              </button>
            </form>
          )}
        </div>
      </div>

      {limitError ? (
        <p
          role="alert"
          className="mt-2 text-[0.8rem] text-[var(--color-danger)]"
          data-testid="monthly-soft-limit-error"
        >
          {limitError}
        </p>
      ) : null}

      <div className="mt-4 grid grid-cols-1 gap-3.5 sm:grid-cols-2 lg:grid-cols-4">
        <div className="rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-surface-2)] p-3.5">
          <div className="text-[0.72rem] font-medium uppercase tracking-[0.06em] text-[var(--color-text-muted)]">
            Monthly Token Usage
          </div>
          <div
            className="mt-1 font-[family-name:var(--font-code)] text-[1.35rem] font-bold text-[var(--color-text-primary)]"
            title={plain.format(totalTokens)}
            data-testid="monthly-total-tokens"
          >
            {usageLoading && !monthUsage ? "…" : fmtTokens(totalTokens)}
          </div>
          <div className="mt-1 text-[0.76rem] text-[var(--color-text-secondary)]">
            {softLimit > 0 ? (
              <span>
                of {fmtTokens(softLimit)} soft limit ({limitPct.toFixed(1)}%)
                {limitSource === "admin" ? " · custom" : ""}
              </span>
            ) : (
              <span>Soft limit: Unlimited (advisory)</span>
            )}
          </div>
          {softLimit > 0 ? (
            <div className="mt-2">
              <div className="h-2 w-full overflow-hidden rounded-full bg-[var(--color-surface-3)]">
                <div
                  className="h-full rounded-full transition-all"
                  style={{
                    width: `${Math.min(100, Math.max(2, limitPct))}%`,
                    backgroundColor:
                      limitPct >= 100
                        ? "var(--color-danger)"
                        : limitPct >= 80
                          ? "var(--color-warning)"
                          : "var(--color-success)",
                  }}
                />
              </div>
              {limitPct >= 100 ? (
                <p className="mt-1 text-[0.72rem] font-medium text-[var(--color-danger)]">
                  Soft allowance exceeded — runs continue without interruption
                </p>
              ) : null}
            </div>
          ) : null}
        </div>

        <div className="rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-surface-2)] p-3.5">
          <div className="text-[0.72rem] font-medium uppercase tracking-[0.06em] text-[var(--color-text-muted)]">
            Included / $0-Cost Share
          </div>
          <div
            className="mt-1 font-[family-name:var(--font-code)] text-[1.35rem] font-bold text-[var(--color-text-primary)]"
            data-testid="monthly-included-share"
          >
            {usageLoading && !monthUsage ? "…" : `${includedSharePct.toFixed(1)}%`}
          </div>
          <div className="mt-1 text-[0.76rem] text-[var(--color-text-secondary)]">
            {fmtTokens(includedTokens)} included · {fmtTokens(billedTokens)} billed
          </div>
        </div>

        <div className="rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-surface-2)] p-3.5">
          <div className="text-[0.72rem] font-medium uppercase tracking-[0.06em] text-[var(--color-text-muted)]">
            Billed Model Spend (MTD)
          </div>
          <div
            className="mt-1 font-[family-name:var(--font-code)] text-[1.35rem] font-bold text-[var(--color-text-primary)]"
            data-testid="monthly-billed-spend"
          >
            {usageLoading && !monthUsage ? "…" : fmtUSD(billedCostUSD)}
          </div>
          <div className="mt-1 text-[0.76rem] text-[var(--color-text-secondary)]">
            Chat {fmtUSD(billedChatCostUSD)} · Tasks{" "}
            {fmtUSD(billedTaskCostUSD)}
          </div>
        </div>

        <div className="rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-surface-2)] p-3.5">
          <div className="text-[0.72rem] font-medium uppercase tracking-[0.06em] text-[var(--color-text-muted)]">
            Prompt Cache Hit Rate
          </div>
          <div
            className="mt-1 font-[family-name:var(--font-code)] text-[1.35rem] font-bold text-[var(--color-text-primary)]"
            data-testid="monthly-cache-hit-rate"
          >
            {usageLoading && !monthUsage ? "…" : `${cacheHitPct.toFixed(1)}%`}
          </div>
          <div className="mt-1 text-[0.76rem] text-[var(--color-text-secondary)]">
            {fmtTokens(monthUsage?.totals?.cached_tokens ?? 0)} cached ·{" "}
            {fmtTokens(monthUsage?.totals?.uncached_tokens ?? 0)} uncached
          </div>
        </div>
      </div>
    </section>
  );
}

const MODEL_RANGES = [
  { value: 7, label: "Last 7 days" },
  { value: 30, label: "Last 30 days" },
  { value: 90, label: "Last 90 days" },
] as const;

function ModelUsageTab() {
  const { models } = useClientConfig();
  const pinnedModels = models?.pinned;
  const [rangeDays, setRangeDays] = useState<number>(30);
  const {
    data: report,
    loading,
    error,
  } = useCancellableFetch(
    useCallback(
      (signal: AbortSignal) => {
        const from = new Date(Date.now() - rangeDays * 24 * 60 * 60 * 1000)
          .toISOString()
          .slice(0, 10);
        return orchestratorApi.usage({ groupBy: "model", from }, signal);
      },
      [rangeDays],
    ),
    [rangeDays],
  );

  const buckets = useMemo(() => {
    const raw = report?.buckets ?? [];
    return [...raw].sort((a, b) => {
      const ta = a.prompt_tokens + a.completion_tokens;
      const tb = b.prompt_tokens + b.completion_tokens;
      if (tb !== ta) return tb - ta;
      return b.cost_usd - a.cost_usd;
    });
  }, [report]);

  const totalTokens =
    (report?.totals?.prompt_tokens ?? 0) + (report?.totals?.completion_tokens ?? 0);
  const totalCost = report?.totals?.cost_usd ?? 0;

  const includedBuckets = buckets.filter(
    (b) =>
      classifyModelBucket(b, pinnedModels) === "included" &&
      b.prompt_tokens + b.completion_tokens > 0,
  );
  const billedBuckets = buckets.filter(
    (b) => classifyModelBucket(b, pinnedModels) === "billed",
  );

  const includedTokens = includedBuckets.reduce(
    (acc, b) => acc + b.prompt_tokens + b.completion_tokens,
    0,
  );
  const billedTokens = billedBuckets.reduce(
    (acc, b) => acc + b.prompt_tokens + b.completion_tokens,
    0,
  );
  const includedRuns = includedBuckets.reduce(
    (acc, b) => acc + b.chat_turns + b.task_iterations,
    0,
  );
  const billedRuns = billedBuckets.reduce(
    (acc, b) => acc + b.chat_turns + b.task_iterations,
    0,
  );
  const includedCost = includedBuckets.reduce((acc, b) => acc + b.cost_usd, 0);
  const billedCost = Math.max(0, totalCost - includedCost);

  const includedPct = totalTokens > 0 ? (includedTokens / totalTokens) * 100 : 0;
  const billedPct = totalTokens > 0 ? (billedTokens / totalTokens) * 100 : 0;

  return (
    <div
      className="section"
      role="region"
      aria-labelledby="modelUsageHeading"
      data-testid="analytics-model-usage"
    >
      <div className="section-header">
        <div>
          <h2 id="modelUsageHeading">Model Mix &amp; Cost Efficiency</h2>
          <p className="mt-0.5 text-[0.82rem] text-[var(--color-text-secondary)]">
            Compare zero-rated / included models against frontier billed models across Chat and Background Tasks.
          </p>
        </div>
        <div className="usage-controls">
          <label htmlFor="modelUsageRange">Range</label>
          <select
            id="modelUsageRange"
            aria-label="Model usage window in days"
            value={rangeDays}
            onChange={(e) => setRangeDays(Number.parseInt(e.target.value, 10))}
          >
            {MODEL_RANGES.map((r) => (
              <option key={r.value} value={r.value}>
                {r.label}
              </option>
            ))}
          </select>
        </div>
      </div>

      {error ? (
        <div className="table-error" role="alert">
          Could not load model usage: {error}
        </div>
      ) : null}

      <div className="mb-5 grid grid-cols-1 gap-4 md:grid-cols-2">
        <div
          className="rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] p-4"
          data-testid="tier-card-included"
        >
          <div className="flex items-center justify-between gap-2">
            <span className="text-[0.76rem] font-semibold uppercase tracking-[0.06em] text-[var(--color-success)]">
              Included / $0-Cost Models
            </span>
            <span className="rounded-full border border-[var(--color-status-success-border)] bg-[var(--color-status-success-bg)] px-2.5 py-0.5 font-[family-name:var(--font-code)] text-[0.74rem] font-semibold text-[var(--color-success)]">
              $0.00 billed
            </span>
          </div>
          <div className="mt-2 flex items-baseline gap-3">
            <span className="font-[family-name:var(--font-code)] text-[1.4rem] font-bold text-[var(--color-text-primary)]">
              {fmtTokens(includedTokens)} tokens
            </span>
            <span className="text-[0.82rem] text-[var(--color-text-secondary)]">
              ({includedPct.toFixed(1)}% of workspace volume)
            </span>
          </div>
          <p className="mt-1 text-[0.78rem] text-[var(--color-text-secondary)]">
            {includedBuckets.length} model{includedBuckets.length === 1 ? "" : "s"} ·{" "}
            {plain.format(includedRuns)} chat turns &amp; task iterations absorbed at $0 direct cost
          </p>
        </div>

        <div
          className="rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] p-4"
          data-testid="tier-card-billed"
        >
          <div className="flex items-center justify-between gap-2">
            <span className="text-[0.76rem] font-semibold uppercase tracking-[0.06em] text-[var(--color-text-secondary)]">
              Frontier / Billed Models
            </span>
            <span className="rounded-full border border-[var(--color-border-strong)] bg-[var(--color-surface-3)] px-2.5 py-0.5 font-[family-name:var(--font-code)] text-[0.74rem] font-semibold text-[var(--color-text-primary)]">
              {fmtUSD(billedCost)} billed
            </span>
          </div>
          <div className="mt-2 flex items-baseline gap-3">
            <span className="font-[family-name:var(--font-code)] text-[1.4rem] font-bold text-[var(--color-text-primary)]">
              {fmtTokens(billedTokens)} tokens
            </span>
            <span className="text-[0.82rem] text-[var(--color-text-secondary)]">
              ({billedPct.toFixed(1)}% of workspace volume)
            </span>
          </div>
          <p className="mt-1 text-[0.78rem] text-[var(--color-text-secondary)]">
            {billedBuckets.length} model{billedBuckets.length === 1 ? "" : "s"} ·{" "}
            {plain.format(billedRuns)} chat turns &amp; task iterations on metered models
          </p>
        </div>
      </div>

      <div
        className="mb-4 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] p-3.5 text-[0.78rem] leading-relaxed text-[var(--color-text-secondary)]"
        data-testid="model-attribution-note"
      >
        <div className="font-semibold text-[var(--color-text-primary)]">
          How Model Attribution, Deleted Chats &amp; Tiers Work
        </div>
        <ul className="mt-1.5 list-disc space-y-1 pl-4">
          <li>
            <strong className="text-[var(--color-text-primary)]">
              How Model Attribution Works:
            </strong>{" "}
            Chat turns are grouped by each conversation&apos;s currently active model setting (whereas background task steps record the exact model used per step). If a conversation&apos;s model is switched later (or bulk-migrated), earlier turns in that conversation are grouped under the conversation&apos;s current model.
          </li>
          <li>
            <strong className="text-[var(--color-text-primary)]">
              Deleted Chats Are Included in Spend &amp; Token Totals:
            </strong>{" "}
            When a user deletes a conversation, its messages are removed, but its completed turn metrics are retained for accurate monthly token and cost accounting and appear under{" "}
            <code className="font-[family-name:var(--font-code)] text-[0.75rem]">
              (deleted chats &amp; default-model turns)
            </code>
            .
          </li>
          <li>
            <strong className="text-[var(--color-text-primary)]">
              Model Tiers &amp; Budget Impact:
            </strong>{" "}
            <strong>Included ($0)</strong> models (pinned with <code className="font-[family-name:var(--font-code)] text-[0.75rem]">free: true</code> or a <code className="font-[family-name:var(--font-code)] text-[0.75rem]">$0</code> pricing override) count 100% toward token volume and the monthly token allowance, and accrue <strong>$0.00</strong> toward billed USD spend and USD budget lines.{" "}
            <strong>Billed</strong> models accrue metered provider or override rates.{" "}
            <strong>No price data</strong> indicates runs where no dollar rate was reported.
          </li>
        </ul>
      </div>

      {loading && !report ? (
        <div className="empty-state">
          <p>Loading model breakdown…</p>
        </div>
      ) : buckets.length === 0 ? (
        <div className="empty-state" data-testid="model-usage-empty">
          <p>No model usage recorded in the last {rangeDays} days.</p>
        </div>
      ) : (
        <div className="table-wrapper overflow-x-auto [-webkit-overflow-scrolling:touch]">
          <table className="tasks-table usage-table" data-testid="model-breakdown-table">
            <thead>
              <tr>
                <th scope="col">Model</th>
                <th scope="col">Tier</th>
                <th scope="col">Token Share</th>
                <th scope="col" className="num">
                  Total Tokens
                </th>
                <th scope="col" className="num">
                  Cache Hit %
                </th>
                <th scope="col" className="num">
                  Chat Turns
                </th>
                <th scope="col" className="num">
                  Task Steps
                </th>
                <th scope="col" className="num">
                  Total Spend
                </th>
                <th scope="col" className="num">
                  Avg / Step
                </th>
              </tr>
            </thead>
            <tbody>
              {buckets.map((b) => {
                const tok = b.prompt_tokens + b.completion_tokens;
                const share = totalTokens > 0 ? (tok / totalTokens) * 100 : 0;
                const tier = classifyModelBucket(b, pinnedModels);
                const isIncluded = tier === "included";
                const isUnpriced = tier === "unpriced";
                const totalSteps = b.chat_turns + b.task_iterations;
                const avgCost = totalSteps > 0 ? b.cost_usd / totalSteps : 0;
                return (
                  <tr key={b.key || "__default__"} data-testid="model-breakdown-row">
                    <td className="usage-key font-[family-name:var(--font-code)] text-[0.8rem]">
                      {modelDisplayName(b, pinnedModels)}
                    </td>
                    <td>
                      {isIncluded ? (
                        <span className="inline-flex items-center rounded-full border border-[var(--color-status-success-border)] bg-[var(--color-status-success-bg)] px-2 py-0.5 text-[0.7rem] font-semibold text-[var(--color-success)]">
                          Included ($0)
                        </span>
                      ) : isUnpriced ? (
                        <span className="inline-flex items-center rounded-full border border-[var(--color-border)] bg-[var(--color-surface-2)] px-2 py-0.5 text-[0.7rem] font-medium text-[var(--color-text-muted)]">
                          No price data
                        </span>
                      ) : (
                        <span className="inline-flex items-center rounded-full border border-[var(--color-border-strong)] bg-[var(--color-surface-3)] px-2 py-0.5 text-[0.7rem] font-medium text-[var(--color-text-secondary)]">
                          Billed
                        </span>
                      )}
                    </td>
                    <td
                      className="min-w-[8rem]"
                      aria-label={`${share.toFixed(1)}% token share`}
                    >
                      <div className="flex items-center gap-2">
                        <div className="h-2 w-20 overflow-hidden rounded-full bg-[var(--color-surface-3)]">
                          <div
                            className="h-full rounded-full"
                            style={{
                              width: `${Math.min(100, Math.max(2, share))}%`,
                              backgroundColor: isIncluded
                                ? "var(--color-success)"
                                : "var(--usage-bar)",
                            }}
                          />
                        </div>
                        <span className="font-[family-name:var(--font-code)] text-[0.76rem] text-[var(--color-text-secondary)]">
                          {share.toFixed(1)}%
                        </span>
                      </div>
                    </td>
                    <td className="num" title={plain.format(tok)}>
                      {fmtTokens(tok)}
                    </td>
                    <td
                      className="num"
                      title={`${plain.format(b.cached_tokens)} cached / ${plain.format(b.uncached_tokens)} uncached`}
                    >
                      {b.prompt_tokens > 0 || b.cached_tokens > 0
                        ? `${b.cache_hit_rate_pct.toFixed(1)}%`
                        : "—"}
                    </td>
                    <td className="num">{plain.format(b.chat_turns)}</td>
                    <td className="num">{plain.format(b.task_iterations)}</td>
                    <td className="num">
                      {isIncluded ? "$0.00" : isUnpriced ? "—" : fmtUSD(b.cost_usd)}
                    </td>
                    <td className="num">
                      {isIncluded ? "$0.00" : isUnpriced || totalSteps === 0 ? "—" : fmtUSD(avgCost)}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

type ConvSort = "cost" | "tokens" | "turns" | "friction";
type TaskSort = "cost" | "tokens" | "iterations" | "failures";

function OutliersAndFrictionTab() {
  const [convSort, setConvSort] = useState<ConvSort>("cost");
  const [taskSort, setTaskSort] = useState<TaskSort>("cost");

  const {
    data: adminStats,
    loading: statsLoading,
    error: statsError,
  } = useCancellableFetch(
    useCallback(async (signal: AbortSignal): Promise<AdminStatsResponse> => {
      const res = await fetch("/api/admin/stats", {
        credentials: "same-origin",
        signal,
      });
      if (!res.ok) {
        throw new Error(`HTTP ${res.status}`);
      }
      return (await res.json()) as AdminStatsResponse;
    }, []),
    [],
  );

  const sortedConversations = useMemo(() => {
    const list = [...(adminStats?.top_conversations ?? [])];
    list.sort((a, b) => {
      if (convSort === "tokens") {
        return (
          b.prompt_tokens +
          b.completion_tokens -
          (a.prompt_tokens + a.completion_tokens)
        );
      }
      if (convSort === "turns") {
        return b.turns - a.turns;
      }
      if (convSort === "friction") {
        const fa =
          a.frustrated_messages + a.struggle_signals + a.advanced_switches;
        const fb =
          b.frustrated_messages + b.struggle_signals + b.advanced_switches;
        if (fb !== fa) return fb - fa;
        return b.turns - a.turns;
      }
      if (b.cost_usd !== a.cost_usd) return b.cost_usd - a.cost_usd;
      return (
        b.prompt_tokens +
        b.completion_tokens -
        (a.prompt_tokens + a.completion_tokens)
      );
    });
    return list.slice(0, 20);
  }, [adminStats, convSort]);

  const sortedTasks = useMemo(() => {
    const list = [...(adminStats?.top_tasks ?? [])];
    const isFailed = (t: TaskOutlierRow) =>
      t.status === "error" ||
      t.status === "dead_lettered" ||
      !!t.error_message;
    list.sort((a, b) => {
      const ta = a.prompt_tokens + a.completion_tokens;
      const tb = b.prompt_tokens + b.completion_tokens;
      if (taskSort === "tokens") {
        if (tb !== ta) return tb - ta;
        return b.cost_usd - a.cost_usd;
      }
      if (taskSort === "iterations") {
        if (b.iterations !== a.iterations) return b.iterations - a.iterations;
        return tb - ta;
      }
      if (taskSort === "failures") {
        const fa = isFailed(a) ? 1 : 0;
        const fb = isFailed(b) ? 1 : 0;
        if (fb !== fa) return fb - fa;
        return b.cost_usd - a.cost_usd;
      }
      if (b.cost_usd !== a.cost_usd) {
        return b.cost_usd - a.cost_usd;
      }
      return tb - ta;
    });
    return list.slice(0, 20);
  }, [adminStats, taskSort]);

  const userSignals = useMemo(() => {
    const list = [...(adminStats?.users ?? [])];
    list.sort((a, b) => {
      const fa =
        a.frustrated_messages + a.struggle_signals + a.advanced_switches;
      const fb =
        b.frustrated_messages + b.struggle_signals + b.advanced_switches;
      if (fb !== fa) return fb - fa;
      return b.total_turns - a.total_turns;
    });
    return list;
  }, [adminStats]);

  return (
    <div className="space-y-6" data-testid="analytics-outliers">
      {/* Glossary / Signal Definitions */}
      <div
        className="rounded-[var(--radius-lg)] border border-[var(--color-border)] bg-[var(--color-surface-1)] p-4 shadow-[var(--shadow-sm)]"
        data-testid="friction-signals-explainer"
      >
        <h2 className="font-heading text-[0.95rem] font-bold text-[var(--color-text-primary)]">
          How Friction &amp; Escalation Signals Work
        </h2>
        <div className="mt-2.5 grid grid-cols-1 gap-3 text-[0.78rem] leading-relaxed text-[var(--color-text-secondary)] sm:grid-cols-2">
          <div className="rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-surface-2)] p-3">
            <div className="font-semibold text-[var(--color-text-primary)]">
              Frustrated (Frustrated Messages)
            </div>
            <p className="mt-0.5">
              User replies that push back on or correct the assistant (for example,{" "}
              <em>&ldquo;that&apos;s wrong&rdquo;</em>, <em>&ldquo;stop&rdquo;</em>,{" "}
              <em>&ldquo;you didn&apos;t follow instructions&rdquo;</em>, or repeated corrections).
            </p>
          </div>
          <div className="rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-surface-2)] p-3">
            <div className="font-semibold text-[var(--color-text-primary)]">
              Struggles (Assistant Struggle Signals)
            </div>
            <p className="mt-0.5">
              Automated detections where the assistant ran into trouble during a turn — repeated tool failures, loop-guard blocks, context compaction, or turns requiring 25+ tool calls.
            </p>
          </div>
          <div className="rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-surface-2)] p-3">
            <div className="font-semibold text-[var(--color-text-primary)]">
              Model Upgrades (Switched up / Model Escalations)
            </div>
            <p className="mt-0.5">
              Times a user accepted the in-chat prompt to switch mid-conversation from a lower-cost or non-fallback model up to the stronger frontier fallback model to finish a harder task.
            </p>
          </div>
          <div className="rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-surface-2)] p-3">
            <div className="font-semibold text-[var(--color-text-primary)]">
              Friction Score &amp; Deleted Chats Scope
            </div>
            <p className="mt-0.5">
              <strong>Friction</strong> is the combined count of{" "}
              <strong>Frustrated + Struggles + Model Upgrades</strong>.{" "}
              <strong>Top Conversations</strong> and <strong>User Escalation Signals</strong> below count <strong>live (non-deleted)</strong> chats, whereas <strong>Chat Spend / Turns</strong> and <strong>Models &amp; Cost</strong> retain completed turn metrics from deleted chats.
            </p>
          </div>
        </div>
      </div>

      {/* Section 1: Top Conversations & Patrols */}
      <div className="section" role="region" aria-labelledby="outlierConvsHeading">
        <div className="section-header">
          <div>
            <h2 id="outlierConvsHeading">
              Top High-Spend, High-Token &amp; High-Friction Chats
            </h2>
            <p className="mt-0.5 text-[0.82rem] text-[var(--color-text-secondary)]">
              Spot long-running patrol threads, heavy context growth, and conversations that triggered model escalations.
            </p>
          </div>
          <div className="usage-controls">
            <label htmlFor="convSortSelect">Rank by</label>
            <select
              id="convSortSelect"
              aria-label="Rank outlier conversations by"
              value={convSort}
              onChange={(e) => setConvSort(e.target.value as ConvSort)}
            >
              <option value="cost">Cost ($ USD)</option>
              <option value="tokens">Total Tokens</option>
              <option value="turns">Turn Count</option>
              <option value="friction">Friction Signals</option>
            </select>
          </div>
        </div>

        {statsError ? (
          <div className="table-error" role="alert">
            Could not load conversation outliers: {statsError}
          </div>
        ) : statsLoading && !adminStats ? (
          <div className="empty-state">
            <p>Loading conversation outliers…</p>
          </div>
        ) : sortedConversations.length === 0 ? (
          <div className="empty-state" data-testid="outliers-conversations-empty">
            <p>No conversations recorded yet.</p>
          </div>
        ) : (
          <div className="table-wrapper overflow-x-auto [-webkit-overflow-scrolling:touch]">
            <table
              className="tasks-table usage-table"
              data-testid="outliers-conversations-table"
            >
              <thead>
                <tr>
                  <th scope="col">Conversation</th>
                  <th scope="col">User</th>
                  <th scope="col">Model</th>
                  <th scope="col" className="num">
                    Turns
                  </th>
                  <th scope="col" className="num">
                    Tokens
                  </th>
                  <th
                    scope="col"
                    className="num"
                    title="Share of prompt tokens served from the provider prompt cache"
                  >
                    Cache Hit %
                  </th>
                  <th
                    scope="col"
                    className="num"
                    title="Combined count of frustrated user messages, assistant struggle signals, and mid-chat model upgrades"
                  >
                    Friction
                  </th>
                  <th scope="col" className="num">
                    Spend
                  </th>
                </tr>
              </thead>
              <tbody>
                {sortedConversations.map((c) => {
                  const totalTok = c.prompt_tokens + c.completion_tokens;
                  const hitRate =
                    c.prompt_tokens > 0
                      ? (c.cached_tokens / c.prompt_tokens) * 100
                      : 0;
                  const isPatrol = /^patrol\b/i.test(c.title.trim());
                  return (
                    <tr
                      key={c.conversation_id}
                      data-testid="outliers-conversation-row"
                    >
                      <td className="max-w-[20rem] truncate">
                        <Link
                          href={`/chat?c=${encodeURIComponent(c.conversation_id)}`}
                          className="font-medium text-[var(--color-accent)] hover:underline"
                        >
                          {c.title || `Chat ${c.conversation_id.slice(0, 8)}`}
                        </Link>
                        {isPatrol ? (
                          <span
                            className="ml-2 inline-flex items-center rounded-full border border-[var(--color-border-strong)] bg-[var(--color-surface-3)] px-2 py-0.5 text-[0.68rem] font-medium text-[var(--color-text-secondary)]"
                            title="Automated recurring patrol conversation"
                          >
                            Patrol
                          </span>
                        ) : null}
                      </td>
                      <td className="font-[family-name:var(--font-code)] text-[0.78rem]">
                        {c.user_email || "(unattributed)"}
                      </td>
                      <td className="font-[family-name:var(--font-code)] text-[0.76rem] text-[var(--color-text-secondary)]">
                        {c.model || "(default model)"}
                      </td>
                      <td className="num">{plain.format(c.turns)}</td>
                      <td className="num" title={plain.format(totalTok)}>
                        {fmtTokens(totalTok)}
                      </td>
                      <td className="num">
                        {c.prompt_tokens > 0 ? `${hitRate.toFixed(0)}%` : "—"}
                      </td>
                      <td
                        className="num text-[0.75rem]"
                        title={`${c.frustrated_messages} frustrated · ${c.struggle_signals} struggles · ${c.advanced_switches} model upgrades`}
                      >
                        {c.frustrated_messages +
                          c.struggle_signals +
                          c.advanced_switches >
                        0
                          ? `${c.frustrated_messages} frustrated · ${c.struggle_signals} struggles · ${c.advanced_switches} upgrade${c.advanced_switches === 1 ? "" : "s"}`
                          : "—"}
                      </td>
                      <td className="num">{fmtUSD(c.cost_usd)}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {/* Section 2: Top Heavy / Failing Background Tasks */}
      <div className="section" role="region" aria-labelledby="outlierTasksHeading">
        <div className="section-header">
          <div>
            <h2 id="outlierTasksHeading">
              Top Heavy, High-Iteration &amp; Failing Background Tasks
            </h2>
            <p className="mt-0.5 text-[0.82rem] text-[var(--color-text-secondary)]">
              Identify scheduled or ad-hoc runs with high token consumption, long tool-call loops, or terminal errors.
            </p>
          </div>
          <div className="usage-controls">
            <label htmlFor="taskSortSelect">Rank by</label>
            <select
              id="taskSortSelect"
              aria-label="Rank background tasks by"
              value={taskSort}
              onChange={(e) => setTaskSort(e.target.value as TaskSort)}
            >
              <option value="cost">Cost ($ USD)</option>
              <option value="tokens">Total Tokens</option>
              <option value="iterations">Iterations Used</option>
              <option value="failures">Failures &amp; Dead Letters</option>
            </select>
          </div>
        </div>

        {statsError ? (
          <div className="table-error" role="alert">
            Could not load background tasks: {statsError}
          </div>
        ) : statsLoading && !adminStats ? (
          <div className="empty-state">
            <p>Loading background task outliers…</p>
          </div>
        ) : sortedTasks.length === 0 ? (
          <div className="empty-state" data-testid="outliers-tasks-empty">
            <p>No background tasks recorded yet.</p>
          </div>
        ) : (
          <div className="table-wrapper overflow-x-auto [-webkit-overflow-scrolling:touch]">
            <table
              className="tasks-table usage-table"
              data-testid="outliers-tasks-table"
            >
              <thead>
                <tr>
                  <th scope="col">Task</th>
                  <th scope="col">Owner</th>
                  <th scope="col">Status</th>
                  <th scope="col">Model</th>
                  <th scope="col" className="num">
                    Iterations
                  </th>
                  <th scope="col" className="num">
                    Tokens
                  </th>
                  <th scope="col" className="num">
                    Spend
                  </th>
                </tr>
              </thead>
              <tbody>
                {sortedTasks.map((t) => {
                  const tok = t.prompt_tokens + t.completion_tokens;
                  const label =
                    t.title.trim() || `Task ${t.task_id.slice(0, 8)}`;
                  return (
                    <tr key={t.task_id} data-testid="outliers-task-row">
                      <td className="max-w-[20rem] truncate">
                        <Link
                          href={`/orchestrator?task=${encodeURIComponent(t.task_id)}`}
                          className="font-medium text-[var(--color-accent)] hover:underline"
                        >
                          {label}
                        </Link>
                        {t.recurrence ? (
                          <span className="ml-2 inline-flex items-center rounded-full border border-[var(--color-border-strong)] bg-[var(--color-surface-3)] px-2 py-0.5 text-[0.68rem] font-medium text-[var(--color-text-secondary)]">
                            {t.recurrence}
                          </span>
                        ) : null}
                      </td>
                      <td className="font-[family-name:var(--font-code)] text-[0.78rem]">
                        {t.owner_email || "(unattributed)"}
                      </td>
                      <td>
                        <span
                          className="font-[family-name:var(--font-code)] text-[0.76rem]"
                          title={t.error_message || undefined}
                        >
                          {t.status}
                        </span>
                      </td>
                      <td className="font-[family-name:var(--font-code)] text-[0.76rem] text-[var(--color-text-secondary)]">
                        {t.model || "(default)"}
                      </td>
                      <td className="num">{plain.format(t.iterations)}</td>
                      <td className="num" title={plain.format(tok)}>
                        {fmtTokens(tok)}
                      </td>
                      <td className="num">{fmtUSD(t.cost_usd)}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {/* Section 3: User Struggle, Frustration & Model-Switch Signals */}
      <div
        className="section"
        role="region"
        aria-labelledby="userSignalsHeading"
      >
        <div className="section-header">
          <div>
            <h2 id="userSignalsHeading">
              User Struggle, Frustration &amp; Model-Switch Signals
            </h2>
            <p className="mt-0.5 text-[0.82rem] text-[var(--color-text-secondary)]">
              Per-user totals across all completed turns: user messages expressing frustration or correction, turns where the assistant hit tool/loop trouble, and mid-chat upgrades to the stronger frontier model.
            </p>
          </div>
        </div>

        {userSignals.length === 0 ? (
          <div className="empty-state" data-testid="outliers-user-signals-empty">
            <p>No user activity signals recorded yet.</p>
          </div>
        ) : (
          <div className="table-wrapper overflow-x-auto [-webkit-overflow-scrolling:touch]">
            <table
              className="tasks-table usage-table"
              data-testid="outliers-user-signals-table"
            >
              <thead>
                <tr>
                  <th scope="col">User</th>
                  <th
                    scope="col"
                    className="num"
                    title="User replies expressing frustration, correction, or pushback"
                  >
                    Frustrated
                  </th>
                  <th
                    scope="col"
                    className="num"
                    title="Turns where the assistant hit repeated tool errors, loop guards, iteration caps, or timeouts"
                  >
                    Struggles
                  </th>
                  <th
                    scope="col"
                    className="num"
                    title="Times the user accepted the in-chat prompt to switch up to the stronger frontier model"
                  >
                    Switched up
                  </th>
                  <th scope="col" className="num" title="Live (non-deleted) conversations owned by this user">
                    Chats
                  </th>
                  <th scope="col" className="num" title="All completed chat turns (including deleted conversations)">
                    Turns
                  </th>
                  <th scope="col" className="num" title="Share of prompt tokens served from the provider prompt cache">
                    Cache Hit %
                  </th>
                  <th scope="col" className="num" title="Total chat spend across all completed turns">
                    Chat Spend
                  </th>
                </tr>
              </thead>
              <tbody>
                {userSignals.map((u) => (
                  <tr key={u.email} data-testid="outliers-user-signal-row">
                    <td className="font-[family-name:var(--font-code)] text-[0.8rem]">
                      {u.email}
                    </td>
                    <td className="num">{u.frustrated_messages}</td>
                    <td className="num">{u.struggle_signals}</td>
                    <td className="num">{u.advanced_switches}</td>
                    <td className="num">{u.conversation_count}</td>
                    <td className="num">{plain.format(u.total_turns)}</td>
                    <td className="num">
                      {u.total_prompt_tokens > 0
                        ? `${u.cache_hit_rate_pct.toFixed(0)}%`
                        : "—"}
                    </td>
                    <td className="num">{fmtUSD(u.total_cost_usd)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  );
}

function AnalyticsInner() {
  const session = useOrchestratorSession();
  const { branding } = useClientConfig();
  const railCollapse = useRailCollapse();
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [tab, setTab] = useState<AnalyticsTab>(initialAnalyticsTab);

  const switchTab = (next: AnalyticsTab) => {
    setTab(next);
    if (typeof window !== "undefined") {
      const url = new URL(window.location.href);
      if (next === "models") url.searchParams.delete("tab");
      else url.searchParams.set("tab", next);
      window.history.replaceState(window.history.state, "", url);
    }
  };

  const isAdmin = session.role === "admin";

  return (
    <div className="grid h-[100dvh] grid-cols-[minmax(0,1fr)] overflow-hidden text-[var(--color-text-primary)] sm:grid-cols-[auto_minmax(0,1fr)]">
      <NavRail
        activeView="analytics"
        brandName={branding.app_name}
        brandLogoSrc={branding.logo_url || undefined}
        sidebarOpen={sidebarOpen}
        setSidebarOpen={setSidebarOpen}
        collapse={railCollapse}
        account={{
          email: session.username ?? "",
          onSignOut: () => void signOutAfter(session.logout()),
        }}
      />

      <main className="flex min-h-0 flex-col overflow-hidden">
        <PageTopBar title="Analytics" onMenu={() => setSidebarOpen(true)} />

        <div className="min-h-0 flex-1 overflow-y-auto [scrollbar-gutter:stable]">
          <div className="container">
            <div
              className="dashboard-content visible"
              data-testid="analytics-dashboard"
            >
              {!session.ready ? (
                <div className="empty-state">
                  <p>Loading analytics…</p>
                </div>
              ) : !session.signedIn || !isAdmin ? (
                <div
                  className="section"
                  role="region"
                  aria-label="Admin access required"
                  data-testid="analytics-admin-required"
                >
                  <h2>Admin access required</h2>
                  <p className="mt-2 text-[0.88rem] text-[var(--color-text-secondary)]">
                    Workspace usage, model mix, and adoption analytics are visible to workspace administrators.
                  </p>
                  <div className="mt-4">
                    <Link href="/chat" className="btn">
                      Back to chat
                    </Link>
                  </div>
                </div>
              ) : (
                <>
                  <MonthlyAllowanceBanner />

                  <div
                    className="dashboard-tabs flex-wrap overflow-x-auto [-webkit-overflow-scrolling:touch]"
                    role="tablist"
                    aria-label="Analytics view"
                  >
                    <button
                      type="button"
                      role="tab"
                      aria-selected={tab === "models"}
                      className={`tab-btn${tab === "models" ? " tab-btn-active" : ""}`}
                      onClick={() => switchTab("models")}
                    >
                      Models &amp; Cost
                    </button>
                    <button
                      type="button"
                      role="tab"
                      aria-selected={tab === "outliers"}
                      className={`tab-btn${tab === "outliers" ? " tab-btn-active" : ""}`}
                      onClick={() => switchTab("outliers")}
                    >
                      Problem Chats &amp; Friction
                    </button>
                    <button
                      type="button"
                      role="tab"
                      aria-selected={tab === "usage"}
                      className={`tab-btn${tab === "usage" ? " tab-btn-active" : ""}`}
                      onClick={() => switchTab("usage")}
                    >
                      Spend &amp; Budgets
                    </button>
                    <button
                      type="button"
                      role="tab"
                      aria-selected={tab === "adoption"}
                      className={`tab-btn${tab === "adoption" ? " tab-btn-active" : ""}`}
                      onClick={() => switchTab("adoption")}
                    >
                      Team Adoption
                    </button>
                    <button
                      type="button"
                      role="tab"
                      aria-selected={tab === "server"}
                      className={`tab-btn${tab === "server" ? " tab-btn-active" : ""}`}
                      onClick={() => switchTab("server")}
                    >
                      Server &amp; Live Agents
                    </button>
                  </div>

                  <div className="dashboard-tab-panel">
                    {tab === "outliers" ? (
                      <OutliersAndFrictionTab />
                    ) : tab === "usage" ? (
                      <UsagePanel />
                    ) : tab === "adoption" ? (
                      <AdoptionPanel />
                    ) : tab === "server" ? (
                      <div className="section" data-testid="analytics-server-tab">
                        <div className="section-header mb-4">
                          <div>
                            <h2 id="serverAgentsHeading">
                              Server Resources &amp; Live Agent Concurrency
                            </h2>
                            <p className="mt-0.5 text-[0.82rem] text-[var(--color-text-secondary)]">
                              Real-time bot and sub-agent execution counts, tool &amp; LLM activity, warm sandboxes, host CPU/memory/disk, and read-only storage overview.
                            </p>
                          </div>
                        </div>
                        <ServerStatsView allowCleanup={false} />
                      </div>
                    ) : (
                      <ModelUsageTab />
                    )}
                  </div>
                </>
              )}
            </div>
          </div>
        </div>
      </main>
    </div>
  );
}

export function AnalyticsPage() {
  return (
    <ToastProvider>
      <AnalyticsInner />
    </ToastProvider>
  );
}

export default AnalyticsPage;
