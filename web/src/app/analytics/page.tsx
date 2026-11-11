import { runStateLabel } from "@/app/shared/ui/StatusChip";
import { formatFixed } from "@/app/lib/i18n";
import { rich } from "@/app/lib/i18n/rich";
import { getLanguage } from "@/app/lib/i18n";
import { t as tr } from "@/app/lib/i18n";
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

const usd = new Intl.NumberFormat(getLanguage(), {
  style: "currency",
  currency: "USD",
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
});
const usdFine = new Intl.NumberFormat(getLanguage(), {
  style: "currency",
  currency: "USD",
  minimumFractionDigits: 2,
  maximumFractionDigits: 4,
});
const compact = new Intl.NumberFormat(getLanguage(), {
  notation: "compact",
  maximumFractionDigits: 1,
});
const plain = new Intl.NumberFormat(getLanguage());

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
      ? tr("{0} (deleted chats & default-model turns)", [b.label])
      : tr("(deleted chats & default-model turns)");
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
    (s) => s.key === "monthly_soft_spend_limit_usd",
  );
  const parsedLimit = limitItem ? Number.parseFloat(limitItem.value) : 0;
  const softLimit =
    Number.isFinite(parsedLimit) && parsedLimit > 0 ? parsedLimit : 0;
  const limitSource: "default" | "admin" = limitItem?.source ?? "default";

  const saveSoftLimit = async (rawValue: string) => {
    setSavingLimit(true);
    setLimitError(null);
    try {
      const cleaned = rawValue.replace(/[$,_\s]/g, "").trim();
      const res = await fetch("/api/admin/settings/monthly_soft_spend_limit_usd", {
        method: "PUT",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ value: cleaned || "0" }),
      });
      if (!res.ok) {
        const text = await res.text();
        throw new Error(text || tr("HTTP {0}", [res.status]));
      }
      setSettingsNonce((n) => n + 1);
      setEditingLimit(false);
    } catch (err) {
      setLimitError(errorMessage(err, tr("Failed to update monthly soft spend limit")));
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
  const limitPct = softLimit > 0 ? (billedCostUSD / softLimit) * 100 : 0;

  return (
    <section
      className="mb-6 rounded-[var(--radius-lg)] border border-[var(--color-border)] bg-[var(--color-surface-1)] p-5 shadow-[var(--shadow-sm)]"
      aria-label={tr("Monthly spend allowance and model mix overview")}
      data-testid="analytics-monthly-banner"
    >
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <p className="text-[0.72rem] font-semibold uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
        {rich("Current UTC Month (since {0})", monthStart)}
      </p>
          <h2 className="mt-0.5 font-heading text-[1.1rem] font-bold text-[var(--color-text-primary)]">
            {tr("Workspace Spend & Model Mix Overview")}</h2>
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
              {softLimit > 0 ? tr("Edit monthly spend limit") : tr("Set monthly spend limit")}
            </button>
          ) : (
            <form
              className="flex flex-wrap items-center gap-2"
              onSubmit={(e) => {
                e.preventDefault();
                void saveSoftLimit(limitDraft);
              }}
            >
              <label htmlFor="monthlySoftSpendLimitInput" className="sr-only">
                {tr("Monthly soft spend limit (USD)")}</label>
              <input
                id="monthlySoftSpendLimitInput"
                type="text"
                inputMode="decimal"
                placeholder={tr("e.g. 500 (USD, 0 = unlimited)")}
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
                {savingLimit ? tr("Saving…") : tr("Save")}
              </button>
              {softLimit > 0 ? (
                <button
                  type="button"
                  disabled={savingLimit}
                  className="btn btn-ghost text-[0.78rem]"
                  onClick={() => void saveSoftLimit("0")}
                >
                  {tr("Clear")}</button>
              ) : null}
              <button
                type="button"
                disabled={savingLimit}
                className="btn btn-ghost text-[0.78rem]"
                onClick={() => setEditingLimit(false)}
              >
                {tr("Cancel")}</button>
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
            {tr("Monthly Token Usage")}</div>
          <div
            className="mt-1 font-[family-name:var(--font-code)] text-[1.35rem] font-bold text-[var(--color-text-primary)]"
            title={plain.format(totalTokens)}
            data-testid="monthly-total-tokens"
          >
            {usageLoading && !monthUsage ? "…" : fmtTokens(totalTokens)}
          </div>
          <div className="mt-1 text-[0.76rem] text-[var(--color-text-secondary)]">
        {rich("{0} input · {1} output", fmtTokens(monthUsage?.totals?.prompt_tokens ?? 0), fmtTokens(monthUsage?.totals?.completion_tokens ?? 0))}
      </div>
        </div>

        <div className="rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-surface-2)] p-3.5">
          <div className="text-[0.72rem] font-medium uppercase tracking-[0.06em] text-[var(--color-text-muted)]">
            {tr("Included / $0-Cost Share")}</div>
          <div
            className="mt-1 font-[family-name:var(--font-code)] text-[1.35rem] font-bold text-[var(--color-text-primary)]"
            data-testid="monthly-included-share"
          >
            {usageLoading && !monthUsage ? "…" : `${formatFixed(includedSharePct, 1)}%`}
          </div>
          <div className="mt-1 text-[0.76rem] text-[var(--color-text-secondary)]">
        {rich("{0} included · {1} billed", fmtTokens(includedTokens), fmtTokens(billedTokens))}
      </div>
        </div>

        <div className="rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-surface-2)] p-3.5">
          <div className="text-[0.72rem] font-medium uppercase tracking-[0.06em] text-[var(--color-text-muted)]">
            {tr("Billed Model Spend (MTD)")}</div>
          <div
            className="mt-1 font-[family-name:var(--font-code)] text-[1.35rem] font-bold text-[var(--color-text-primary)]"
            data-testid="monthly-billed-spend"
          >
            {usageLoading && !monthUsage ? "…" : fmtUSD(billedCostUSD)}
          </div>
          <div className="mt-1 text-[0.76rem] text-[var(--color-text-secondary)]">
        {rich("Chat {0} · Tasks {1}", fmtUSD(billedChatCostUSD), fmtUSD(billedTaskCostUSD))}
      </div>
          <div className="mt-1 text-[0.76rem] text-[var(--color-text-secondary)]">
            {softLimit > 0 ? (
              <span>
        {rich("of {0} soft spend limit ({1}%){2}", fmtUSD(softLimit), formatFixed(limitPct, 1), limitSource === "admin" ? tr(" · custom") : "")}
      </span>
            ) : (
              <span>{tr("Soft spend limit: Unlimited (advisory)")}</span>
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
                  {tr("Soft spend limit exceeded — runs continue without interruption")}</p>
              ) : null}
            </div>
          ) : null}
        </div>

        <div className="rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-surface-2)] p-3.5">
          <div className="text-[0.72rem] font-medium uppercase tracking-[0.06em] text-[var(--color-text-muted)]">
            {tr("Prompt Cache Hit Rate")}</div>
          <div
            className="mt-1 font-[family-name:var(--font-code)] text-[1.35rem] font-bold text-[var(--color-text-primary)]"
            data-testid="monthly-cache-hit-rate"
          >
            {usageLoading && !monthUsage ? "…" : `${formatFixed(cacheHitPct, 1)}%`}
          </div>
          <div className="mt-1 text-[0.76rem] text-[var(--color-text-secondary)]">
        {rich("{0} cached · {1} uncached", fmtTokens(monthUsage?.totals?.cached_tokens ?? 0), fmtTokens(monthUsage?.totals?.uncached_tokens ?? 0))}
      </div>
        </div>
      </div>
    </section>
  );
}

const MODEL_RANGES = [
  { value: 7, label: tr("Last 7 days") },
  { value: 30, label: tr("Last 30 days") },
  { value: 90, label: tr("Last 90 days") },
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
          <h2 id="modelUsageHeading">{tr("Model Mix & Cost Efficiency")}</h2>
          <p className="mt-0.5 text-[0.82rem] text-[var(--color-text-secondary)]">
            {tr("Compare zero-rated / included models against frontier billed models across Chat and Background Tasks.")}</p>
        </div>
        <div className="usage-controls">
          <label htmlFor="modelUsageRange">{tr("Range")}</label>
          <select
            id="modelUsageRange"
            aria-label={tr("Model usage window in days")}
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
        {rich("Could not load model usage: {0}", error)}
      </div>
      ) : null}

      <div className="mb-5 grid grid-cols-1 gap-4 md:grid-cols-2">
        <div
          className="rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] p-4"
          data-testid="tier-card-included"
        >
          <div className="flex items-center justify-between gap-2">
            <span className="text-[0.76rem] font-semibold uppercase tracking-[0.06em] text-[var(--color-success)]">
              {tr("Included / $0-Cost Models")}</span>
            <span className="rounded-full border border-[var(--color-status-success-border)] bg-[var(--color-status-success-bg)] px-2.5 py-0.5 font-[family-name:var(--font-code)] text-[0.74rem] font-semibold text-[var(--color-success)]">
              {tr("$0.00 billed")}</span>
          </div>
          <div className="mt-2 flex items-baseline gap-3">
            <span className="font-[family-name:var(--font-code)] text-[1.4rem] font-bold text-[var(--color-text-primary)]">
        {rich("{0} tokens", fmtTokens(includedTokens))}
      </span>
            <span className="text-[0.82rem] text-[var(--color-text-secondary)]">
        {rich("({0}% of workspace volume)", formatFixed(includedPct, 1))}
      </span>
          </div>
          <p className="mt-1 text-[0.78rem] text-[var(--color-text-secondary)]">
        {rich("{0} · {1} chat turns & task iterations absorbed at $0 direct cost", tr(includedBuckets.length === 1 ? "{0} model" : "{0} models", [includedBuckets.length]), plain.format(includedRuns))}
      </p>
        </div>

        <div
          className="rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] p-4"
          data-testid="tier-card-billed"
        >
          <div className="flex items-center justify-between gap-2">
            <span className="text-[0.76rem] font-semibold uppercase tracking-[0.06em] text-[var(--color-text-secondary)]">
              {tr("Frontier / Billed Models")}</span>
            <span className="rounded-full border border-[var(--color-border-strong)] bg-[var(--color-surface-3)] px-2.5 py-0.5 font-[family-name:var(--font-code)] text-[0.74rem] font-semibold text-[var(--color-text-primary)]">
        {rich("{0} billed", fmtUSD(billedCost))}
      </span>
          </div>
          <div className="mt-2 flex items-baseline gap-3">
            <span className="font-[family-name:var(--font-code)] text-[1.4rem] font-bold text-[var(--color-text-primary)]">
        {rich("{0} tokens", fmtTokens(billedTokens))}
      </span>
            <span className="text-[0.82rem] text-[var(--color-text-secondary)]">
        {rich("({0}% of workspace volume)", formatFixed(billedPct, 1))}
      </span>
          </div>
          <p className="mt-1 text-[0.78rem] text-[var(--color-text-secondary)]">
        {rich("{0} · {1} chat turns & task iterations on metered models", tr(billedBuckets.length === 1 ? "{0} model" : "{0} models", [billedBuckets.length]), plain.format(billedRuns))}
      </p>
        </div>
      </div>

      <div
        className="mb-4 rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-2)] p-3.5 text-[0.78rem] leading-relaxed text-[var(--color-text-secondary)]"
        data-testid="model-attribution-note"
      >
        <div className="font-semibold text-[var(--color-text-primary)]">
          {tr("How Model Attribution, Deleted Chats & Tiers Work")}</div>
        <ul className="mt-1.5 list-disc space-y-1 pl-4">
          <li>
        {rich("{0} Chat turns are grouped by each conversation's currently active model setting (whereas background task steps record the exact model used per step). If a conversation's model is switched later (or bulk-migrated), earlier turns in that conversation are grouped under the conversation's current model.", <strong className="text-[var(--color-text-primary)]">
              {tr("How Model Attribution Works:")}</strong>)}
      </li>
          <li>
        {rich("{0} When a user deletes a conversation, its messages are removed, but its completed turn metrics are retained for accurate monthly token and cost accounting and appear under {1}.", <strong className="text-[var(--color-text-primary)]">
              {tr("Deleted Chats Are Included in Spend & Token Totals:")}</strong>, <code className="font-[family-name:var(--font-code)] text-[0.75rem]">
              (deleted chats &amp; default-model turns)
            </code>)}
      </li>
          <li>
        {rich("{0} {1} models (pinned with {2} or a {3} pricing override) count 100% toward token volume and the monthly token allowance, and accrue {4} toward billed USD spend and USD budget lines. {5} models accrue metered provider or override rates. {6} indicates runs where no dollar rate was reported.", <strong className="text-[var(--color-text-primary)]">
              {tr("Model Tiers & Budget Impact:")}</strong>, <strong>{tr("Included ($0)")}</strong>, <code className="font-[family-name:var(--font-code)] text-[0.75rem]">free: true</code>, <code className="font-[family-name:var(--font-code)] text-[0.75rem]">$0</code>, <strong>$0.00</strong>, <strong>{tr("Billed")}</strong>, <strong>{tr("No price data")}</strong>)}
      </li>
        </ul>
      </div>

      {loading && !report ? (
        <div className="empty-state">
          <p>{tr("Loading model breakdown…")}</p>
        </div>
      ) : buckets.length === 0 ? (
        <div className="empty-state" data-testid="model-usage-empty">
          <p>
        {rich("No model usage recorded in the last {0} days.", rangeDays)}
      </p>
        </div>
      ) : (
        <div className="table-wrapper overflow-x-auto [-webkit-overflow-scrolling:touch]">
          <table className="tasks-table usage-table" data-testid="model-breakdown-table">
            <thead>
              <tr>
                <th scope="col">{tr("Model")}</th>
                <th scope="col">{tr("Tier")}</th>
                <th scope="col">{tr("Token Share")}</th>
                <th scope="col" className="num">
                  {tr("Total Tokens")}</th>
                <th scope="col" className="num">
                  {tr("Cache Hit %")}</th>
                <th scope="col" className="num">
                  {tr("Chat Turns")}</th>
                <th scope="col" className="num">
                  {tr("Task Steps")}</th>
                <th scope="col" className="num">
                  {tr("Total Spend")}</th>
                <th scope="col" className="num">
                  {tr("Avg / Step")}</th>
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
                          {tr("Included ($0)")}</span>
                      ) : isUnpriced ? (
                        <span className="inline-flex items-center rounded-full border border-[var(--color-border)] bg-[var(--color-surface-2)] px-2 py-0.5 text-[0.7rem] font-medium text-[var(--color-text-muted)]">
                          {tr("No price data")}</span>
                      ) : (
                        <span className="inline-flex items-center rounded-full border border-[var(--color-border-strong)] bg-[var(--color-surface-3)] px-2 py-0.5 text-[0.7rem] font-medium text-[var(--color-text-secondary)]">
                          {tr("Billed")}</span>
                      )}
                    </td>
                    <td
                      className="min-w-[8rem]"
                      aria-label={tr("{0}% token share", [formatFixed(share, 1)])}
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
                          {formatFixed(share, 1)}%
                        </span>
                      </div>
                    </td>
                    <td className="num" title={plain.format(tok)}>
                      {fmtTokens(tok)}
                    </td>
                    <td
                      className="num"
                      title={tr("{0} cached / {1} uncached", [plain.format(b.cached_tokens), plain.format(b.uncached_tokens)])}
                    >
                      {b.prompt_tokens > 0 || b.cached_tokens > 0
                        ? `${formatFixed(b.cache_hit_rate_pct, 1)}%`
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
        throw new Error(tr("HTTP {0}", [res.status]));
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
          {tr("How Friction & Escalation Signals Work")}</h2>
        <div className="mt-2.5 grid grid-cols-1 gap-3 text-[0.78rem] leading-relaxed text-[var(--color-text-secondary)] sm:grid-cols-2">
          <div className="rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-surface-2)] p-3">
            <div className="font-semibold text-[var(--color-text-primary)]">
              {tr("Frustrated (Frustrated Messages)")}</div>
            <p className="mt-0.5">
        {rich("User replies that push back on or correct the assistant (for example, {0}, {1}, {2}, or repeated corrections).", <em>{tr("“that's wrong”")}</em>, <em>{tr("“stop”")}</em>, <em>{tr("“you didn't follow instructions”")}</em>)}
      </p>
          </div>
          <div className="rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-surface-2)] p-3">
            <div className="font-semibold text-[var(--color-text-primary)]">
              {tr("Struggles (Assistant Struggle Signals)")}</div>
            <p className="mt-0.5">
              {tr("Automated detections where the assistant ran into trouble during a turn — repeated tool failures, loop-guard blocks, context compaction, or turns requiring 25+ tool calls.")}</p>
          </div>
          <div className="rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-surface-2)] p-3">
            <div className="font-semibold text-[var(--color-text-primary)]">
              {tr("Model Upgrades (Switched up / Model Escalations)")}</div>
            <p className="mt-0.5">
              {tr("Times a user accepted the in-chat prompt to switch mid-conversation from a lower-cost or non-fallback model up to the stronger frontier fallback model to finish a harder task.")}</p>
          </div>
          <div className="rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-surface-2)] p-3">
            <div className="font-semibold text-[var(--color-text-primary)]">
              {tr("Friction Score & Deleted Chats Scope")}</div>
            <p className="mt-0.5">
        {rich("{0} is the combined count of {1}. {2} and {3} below count {4} chats, whereas {5} and {6} retain completed turn metrics from deleted chats.", <strong>{tr("Friction")}</strong>, <strong>{tr("Frustrated + Struggles + Model Upgrades")}</strong>, <strong>{tr("Top Conversations")}</strong>, <strong>{tr("User Escalation Signals")}</strong>, <strong>{tr("live (non-deleted)")}</strong>, <strong>{tr("Chat Spend / Turns")}</strong>, <strong>{tr("Models & Cost")}</strong>)}
      </p>
          </div>
        </div>
      </div>

      {/* Section 1: Top Conversations & Patrols */}
      <div className="section" role="region" aria-labelledby="outlierConvsHeading">
        <div className="section-header">
          <div>
            <h2 id="outlierConvsHeading">
              {tr("Top High-Spend, High-Token & High-Friction Chats")}
            </h2>
            <p className="mt-0.5 text-[0.82rem] text-[var(--color-text-secondary)]">
              {tr("Spot long-running patrol threads, heavy context growth, and conversations that triggered model escalations.")}
            </p>
          </div>
          <div className="usage-controls">
            <label htmlFor="convSortSelect">{tr("Rank by")}</label>
            <select
              id="convSortSelect"
              aria-label={tr("Rank outlier conversations by")}
              value={convSort}
              onChange={(e) => setConvSort(e.target.value as ConvSort)}
            >
              <option value="cost">{tr("Cost ($ USD)")}</option>
              <option value="tokens">{tr("Total Tokens")}</option>
              <option value="turns">{tr("Turn Count")}</option>
              <option value="friction">{tr("Friction Signals")}</option>
            </select>
          </div>
        </div>

        {statsError ? (
          <div className="table-error" role="alert">
        {rich("Could not load conversation outliers: {0}", statsError)}
      </div>
        ) : statsLoading && !adminStats ? (
          <div className="empty-state">
            <p>{tr("Loading conversation outliers…")}</p>
          </div>
        ) : sortedConversations.length === 0 ? (
          <div className="empty-state" data-testid="outliers-conversations-empty">
            <p>{tr("No conversations recorded yet.")}</p>
          </div>
        ) : (
          <div className="table-wrapper overflow-x-auto [-webkit-overflow-scrolling:touch]">
            <table
              className="tasks-table usage-table"
              data-testid="outliers-conversations-table"
            >
              <thead>
                <tr>
                  <th scope="col">{tr("Conversation")}</th>
                  <th scope="col">{tr("User")}</th>
                  <th scope="col">{tr("Model")}</th>
                  <th scope="col" className="num">
                    {tr("Turns")}
                  </th>
                  <th scope="col" className="num">
                    {tr("Tokens")}
                  </th>
                  <th
                    scope="col"
                    className="num"
                    title={tr("Share of prompt tokens served from the provider prompt cache")}
                  >
                    {tr("Cache Hit %")}</th>
                  <th
                    scope="col"
                    className="num"
                    title={tr("Combined count of frustrated user messages, assistant struggle signals, and mid-chat model upgrades")}
                  >
                    {tr("Friction")}</th>
                  <th scope="col" className="num">
                    {tr("Spend")}
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
                          {c.title || tr("Chat {0}", [c.conversation_id.slice(0, 8)])}
                        </Link>
                        {isPatrol ? (
                          <span
                            className="ml-2 inline-flex items-center rounded-full border border-[var(--color-border-strong)] bg-[var(--color-surface-3)] px-2 py-0.5 text-[0.68rem] font-medium text-[var(--color-text-secondary)]"
                            title={tr("Automated recurring patrol conversation")}
                          >
                            {tr("Patrol")}
                          </span>
                        ) : null}
                      </td>
                      <td className="font-[family-name:var(--font-code)] text-[0.78rem]">
                        {c.user_email || tr("(unattributed)")}
                      </td>
                      <td className="font-[family-name:var(--font-code)] text-[0.76rem] text-[var(--color-text-secondary)]">
                        {c.model || tr("(default model)")}
                      </td>
                      <td className="num">{plain.format(c.turns)}</td>
                      <td className="num" title={plain.format(totalTok)}>
                        {fmtTokens(totalTok)}
                      </td>
                      <td className="num">
                        {c.prompt_tokens > 0 ? `${formatFixed(hitRate, 0)}%` : "—"}
                      </td>
                      <td
                        className="num text-[0.75rem]"
                        title={tr("{0} frustrated · {1} struggles · {2} model upgrades", [c.frustrated_messages, c.struggle_signals, c.advanced_switches])}
                      >
                        {c.frustrated_messages +
                          c.struggle_signals +
                          c.advanced_switches >
                        0
                          ? tr(c.advanced_switches === 1 ? "{0} frustrated · {1} struggles · {2} upgrade" : "{0} frustrated · {1} struggles · {2} upgrades", [c.frustrated_messages, c.struggle_signals, c.advanced_switches])
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
              {tr("Top Heavy, High-Iteration & Failing Background Tasks")}
            </h2>
            <p className="mt-0.5 text-[0.82rem] text-[var(--color-text-secondary)]">
              {tr("Identify scheduled or ad-hoc runs with high token consumption, long tool-call loops, or terminal errors.")}
            </p>
          </div>
          <div className="usage-controls">
            <label htmlFor="taskSortSelect">{tr("Rank by")}</label>
            <select
              id="taskSortSelect"
              aria-label={tr("Rank background tasks by")}
              value={taskSort}
              onChange={(e) => setTaskSort(e.target.value as TaskSort)}
            >
              <option value="cost">{tr("Cost ($ USD)")}</option>
              <option value="tokens">{tr("Total Tokens")}</option>
              <option value="iterations">{tr("Iterations Used")}</option>
              <option value="failures">{tr("Failures & Dead Letters")}</option>
            </select>
          </div>
        </div>

        {statsError ? (
          <div className="table-error" role="alert">
        {rich("Could not load background tasks: {0}", statsError)}
      </div>
        ) : statsLoading && !adminStats ? (
          <div className="empty-state">
            <p>{tr("Loading background task outliers…")}</p>
          </div>
        ) : sortedTasks.length === 0 ? (
          <div className="empty-state" data-testid="outliers-tasks-empty">
            <p>{tr("No background tasks recorded yet.")}</p>
          </div>
        ) : (
          <div className="table-wrapper overflow-x-auto [-webkit-overflow-scrolling:touch]">
            <table
              className="tasks-table usage-table"
              data-testid="outliers-tasks-table"
            >
              <thead>
                <tr>
                  <th scope="col">{tr("Task")}</th>
                  <th scope="col">{tr("Owner")}</th>
                  <th scope="col">{tr("Status")}</th>
                  <th scope="col">{tr("Model")}</th>
                  <th scope="col" className="num">
                    {tr("Iterations")}
                  </th>
                  <th scope="col" className="num">
                    {tr("Tokens")}
                  </th>
                  <th scope="col" className="num">
                    {tr("Spend")}
                  </th>
                </tr>
              </thead>
              <tbody>
                {sortedTasks.map((t) => {
                  const tok = t.prompt_tokens + t.completion_tokens;
                  const label =
                    t.title.trim() || tr("Task {0}", [t.task_id.slice(0, 8)]);
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
                        {t.owner_email || tr("(unattributed)")}
                      </td>
                      <td>
                        <span
                          className="font-[family-name:var(--font-code)] text-[0.76rem]"
                          title={t.error_message || undefined}
                        >
                          {runStateLabel(t.status)}
                        </span>
                      </td>
                      <td className="font-[family-name:var(--font-code)] text-[0.76rem] text-[var(--color-text-secondary)]">
                        {t.model || tr("(default)")}
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
              {tr("User Struggle, Frustration & Model-Switch Signals")}
            </h2>
            <p className="mt-0.5 text-[0.82rem] text-[var(--color-text-secondary)]">
              {tr("Per-user totals across all completed turns: user messages expressing frustration or correction, turns where the assistant hit tool/loop trouble, and mid-chat upgrades to the stronger frontier model.")}
            </p>
          </div>
        </div>

        {userSignals.length === 0 ? (
          <div className="empty-state" data-testid="outliers-user-signals-empty">
            <p>{tr("No user activity signals recorded yet.")}</p>
          </div>
        ) : (
          <div className="table-wrapper overflow-x-auto [-webkit-overflow-scrolling:touch]">
            <table
              className="tasks-table usage-table"
              data-testid="outliers-user-signals-table"
            >
              <thead>
                <tr>
                  <th scope="col">{tr("User")}</th>
                  <th
                    scope="col"
                    className="num"
                    title={tr("User replies expressing frustration, correction, or pushback")}
                  >
                    {tr("Frustrated")}
                  </th>
                  <th
                    scope="col"
                    className="num"
                    title={tr("Turns where the assistant hit repeated tool errors, loop guards, iteration caps, or timeouts")}
                  >
                    {tr("Struggles")}
                  </th>
                  <th
                    scope="col"
                    className="num"
                    title={tr("Times the user accepted the in-chat prompt to switch up to the stronger frontier model")}
                  >
                    {tr("Switched up")}
                  </th>
                  <th scope="col" className="num" title={tr("Live (non-deleted) conversations owned by this user")}>
                    {tr("Chats")}
                  </th>
                  <th scope="col" className="num" title={tr("All completed chat turns (including deleted conversations)")}>
                    {tr("Turns")}
                  </th>
                  <th scope="col" className="num" title={tr("Share of prompt tokens served from the provider prompt cache")}>
                    {tr("Cache Hit %")}</th>
                  <th scope="col" className="num" title={tr("Total chat spend across all completed turns")}>
                    {tr("Chat Spend")}
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
                        ? `${formatFixed(u.cache_hit_rate_pct, 0)}%`
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
        <PageTopBar title={tr("Analytics")} onMenu={() => setSidebarOpen(true)} />

        <div className="min-h-0 flex-1 overflow-y-auto [scrollbar-gutter:stable]">
          <div className="container">
            <div
              className="dashboard-content visible"
              data-testid="analytics-dashboard"
            >
              {!session.ready ? (
                <div className="empty-state">
                  <p>{tr("Loading analytics…")}</p>
                </div>
              ) : !session.signedIn || !isAdmin ? (
                <div
                  className="section"
                  role="region"
                  aria-label={tr("Admin access required")}
                  data-testid="analytics-admin-required"
                >
                  <h2>{tr("Admin access required")}</h2>
                  <p className="mt-2 text-[0.88rem] text-[var(--color-text-secondary)]">
                    {tr("Workspace usage, model mix, and adoption analytics are visible to workspace administrators.")}
                  </p>
                  <div className="mt-4">
                    <Link href="/chat" className="btn">
                      {tr("Back to chat")}
                    </Link>
                  </div>
                </div>
              ) : (
                <>
                  <MonthlyAllowanceBanner />

                  <div
                    className="dashboard-tabs flex-wrap overflow-x-auto [-webkit-overflow-scrolling:touch]"
                    role="tablist"
                    aria-label={tr("Analytics view")}
                  >
                    <button
                      type="button"
                      role="tab"
                      aria-selected={tab === "models"}
                      className={`tab-btn${tab === "models" ? " tab-btn-active" : ""}`}
                      onClick={() => switchTab("models")}
                    >
                      {tr("Models & Cost")}
                    </button>
                    <button
                      type="button"
                      role="tab"
                      aria-selected={tab === "outliers"}
                      className={`tab-btn${tab === "outliers" ? " tab-btn-active" : ""}`}
                      onClick={() => switchTab("outliers")}
                    >
                      {tr("Problem Chats & Friction")}
                    </button>
                    <button
                      type="button"
                      role="tab"
                      aria-selected={tab === "usage"}
                      className={`tab-btn${tab === "usage" ? " tab-btn-active" : ""}`}
                      onClick={() => switchTab("usage")}
                    >
                      {tr("Spend & Budgets")}
                    </button>
                    <button
                      type="button"
                      role="tab"
                      aria-selected={tab === "adoption"}
                      className={`tab-btn${tab === "adoption" ? " tab-btn-active" : ""}`}
                      onClick={() => switchTab("adoption")}
                    >
                      {tr("Team Adoption")}
                    </button>
                    <button
                      type="button"
                      role="tab"
                      aria-selected={tab === "server"}
                      className={`tab-btn${tab === "server" ? " tab-btn-active" : ""}`}
                      onClick={() => switchTab("server")}
                    >
                      {tr("Server & Live Agents")}
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
                              {tr("Server Resources & Live Agent Concurrency")}
                            </h2>
                            <p className="mt-0.5 text-[0.82rem] text-[var(--color-text-secondary)]">
                              {tr("Real-time bot and sub-agent execution counts, tool & LLM activity, warm sandboxes, host CPU/memory/disk, and read-only storage overview.")}
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
