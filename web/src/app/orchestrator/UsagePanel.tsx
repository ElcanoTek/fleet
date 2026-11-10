import { useCallback, useState } from "react";
import {
  orchestratorApi,
  type BudgetCreate,
  type BudgetStatus,
  type UsageBucket,
  type UsageGroupBy,
  type UsageReport,
} from "@/app/shared/lib/orchestratorApi";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";
import { downloadFile } from "@/app/shared/lib/downloadFile";
import { plural } from "@/app/shared/lib/format";
import { errorMessage } from "@/app/shared/lib/errorMessage";

// UsagePanel — the Operations Center Usage tab (#601 part 1): cost/token
// roll-ups over the persisted metering (task iterations + chat turns), grouped
// by principal, API key, project, model, or day/week time bucket. Driven by
// GET /admin/usage (admin-only).
//
// Dataviz notes: this is a single-measure magnitude chart, so it uses ONE hue
// (--usage-bar, validated per theme for ≥3:1 contrast, in-band lightness and
// the chroma floor) — never a categorical palette. Bars are thin with a 4px
// rounded data-end (square at the baseline), values are direct-labeled at bar
// tips for entity groupings, and the full table view below the chart carries
// every value, so the chart never gates the data. While a refetch is in
// flight the previous render is held at reduced opacity (no skeleton flash).
//
// Honest scope (#289): dollar totals depend on pricing config — native-provider
// runs accrue $0 unless a pricing override is set — so the panel always shows
// token totals alongside dollars and surfaces the server's note verbatim.

const RANGE_OPTIONS = [7, 30, 90] as const;

const GROUP_OPTIONS: { value: UsageGroupBy; label: string }[] = [
  { value: "user", label: "User" },
  { value: "key", label: "API key" },
  { value: "project", label: "Project" },
  { value: "model", label: "Model" },
  { value: "day", label: "Day" },
  { value: "week", label: "Week" },
];

// Past this many entity buckets the tail folds into "Other" — more bars stop
// being readable and the table below still lists every bucket.
const MAX_BARS = 11;

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
const compact = new Intl.NumberFormat("en-US", { notation: "compact", maximumFractionDigits: 1 });
const plain = new Intl.NumberFormat("en-US");

function fmtUSD(v: number): string {
  // Sub-cent spend is real at per-iteration granularity — keep enough
  // precision that a $0.004 run doesn't render as the misleading "$0.00".
  return v > 0 && v < 0.01 ? usdFine.format(v) : usd.format(v);
}

// bucketDisplayName renders the empty bucket key honestly per dimension: rows
// without a project/key/creator are real spend, not an error.
function bucketDisplayName(b: UsageBucket, groupBy: UsageGroupBy): string {
  if (b.key === "") {
    switch (groupBy) {
      case "user":
        return "(unattributed)";
      case "key":
        return "(no API key)";
      case "project":
        return "(no project)";
      case "model":
        return "(deleted chats & default-model turns)";
      default:
        return "(none)";
    }
  }
  if (groupBy === "project" && b.label) return b.label;
  return b.key;
}

export function UsagePanel() {
  const [rangeDays, setRangeDays] = useState<number>(30);
  // A failed CSV download is shown in place (the button used to navigate).
  const [downloadError, setDownloadError] = useState<string | null>(null);
  const [groupBy, setGroupBy] = useState<UsageGroupBy>("user");
  const [measure, setMeasure] = useState<"cost" | "tokens" | "cache">("cost");

  const {
    data: report,
    loading,
    error,
  } = useCancellableFetch(
    // `from` is computed inside the fetcher (an effect-driven callback, where
    // reading the clock is fine) rather than in render, keeping render pure.
    useCallback((signal: AbortSignal) => {
      const from = new Date(Date.now() - rangeDays * 24 * 60 * 60 * 1000)
        .toISOString()
        .slice(0, 10);
      return orchestratorApi.usage({ groupBy, from }, signal);
    }, [groupBy, rangeDays]),
    [groupBy, rangeDays],
  );

  return (
    <div className="section" role="region" aria-labelledby="usageHeading">
      <div className="section-header">
        <h2 id="usageHeading">Usage &amp; Spend</h2>
        {/* One filter row scoping everything below; date range first. */}
        <div className="usage-controls">
          <label htmlFor="usageRange">Range</label>
          <select
            id="usageRange"
            aria-label="Usage window in days"
            value={rangeDays}
            onChange={(e) => setRangeDays(Number.parseInt(e.target.value, 10))}
          >
            {RANGE_OPTIONS.map((d) => (
              <option key={d} value={d}>
                Last {d} days
              </option>
            ))}
          </select>
          <label htmlFor="usageGroupBy">Group by</label>
          <select
            id="usageGroupBy"
            aria-label="Usage grouping dimension"
            value={groupBy}
            onChange={(e) => setGroupBy(e.target.value as UsageGroupBy)}
          >
            {GROUP_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
          <label htmlFor="usageMeasure">Measure</label>
          <select
            id="usageMeasure"
            aria-label="Chart measure"
            value={measure}
            onChange={(e) => setMeasure(e.target.value as "cost" | "tokens" | "cache")}
          >
            <option value="cost">Cost (USD)</option>
            <option value="tokens">Tokens</option>
            <option value="cache">Cache hit %</option>
          </select>
          <button
            type="button"
            className="btn"
            aria-label="Download the current usage report as CSV"
            data-testid="usage-download-csv"
            onClick={() => {
              // Clock read + navigation in the click handler (not render) so the
              // component body stays pure; matches the fetcher's `from` window.
              const from = new Date(Date.now() - rangeDays * 24 * 60 * 60 * 1000)
                .toISOString()
                .slice(0, 10);
              const qs = new URLSearchParams({ format: "csv", group_by: groupBy, from });
              // Fetch-then-save (downloadFile) instead of navigating: a 401/500
              // used to replace the dashboard with the server's error body.
              void downloadFile(`/api/orchestrator/admin/usage?${qs.toString()}`, `usage-${groupBy}-${from}.csv`).catch(
                (err: unknown) => setDownloadError(errorMessage(err, "download failed")),
              );
            }}
          >
            Download CSV
          </button>
        </div>
      </div>

      {downloadError ? (
        <div className="table-error" role="alert" data-testid="usage-download-error">
          Couldn&apos;t download the CSV: {downloadError}
        </div>
      ) : null}
      {error ? (
        <div className="table-error">Failed to load usage report: {error}</div>
      ) : !report && loading ? (
        <div className="loading">
          <p>Loading usage report…</p>
        </div>
      ) : !report ? null : (
        <div className={loading ? "usage-refetching" : undefined}>
          <UsageTotals report={report} />
          <p className="usage-note" data-testid="usage-note">
            {report.note}
          </p>
          {report.buckets.length === 0 ? (
            <div className="table-empty">No recorded usage in this window.</div>
          ) : (
            <>
              <UsageChart report={report} measure={measure} />
              <UsageTable report={report} />
            </>
          )}
          <p className="refresh-note">
            {new Date(report.from).toISOString().slice(0, 10)} →{" "}
            {new Date(report.to).toISOString().slice(0, 10)} · sources: {report.sources.join(" + ")}{" "}
            · {plural(report.buckets.length, "bucket")}
          </p>
        </div>
      )}
      <BudgetsSection />
    </div>
  );
}

// BudgetsSection — per-principal rolling budgets (#601 part 2) that gate task
// creation over this same metering. Create/delete live here (and on
// `fleet sched budget`); leftover project-scope rows still list but cannot
// be created.
function BudgetsSection() {
  const { data, error, reload } = useCancellableFetch(
    useCallback((signal: AbortSignal) => orchestratorApi.budgets(signal), []),
    [],
  );
  const [scope, setScope] = useState<BudgetCreate["scope"]>("user");
  const [principal, setPrincipal] = useState("");
  const [windowKind, setWindowKind] = useState<BudgetCreate["window"]>("month");
  const [softUsd, setSoftUsd] = useState("");
  const [hardUsd, setHardUsd] = useState("");
  const [softTokens, setSoftTokens] = useState("");
  const [hardTokens, setHardTokens] = useState("");
  const [saving, setSaving] = useState(false);
  const [deleting, setDeleting] = useState<string | null>(null);
  const [formError, setFormError] = useState<string | null>(null);

  const submit = async () => {
    if (saving) return;
    setFormError(null);
    const body: BudgetCreate = {
      scope,
      principal_id: principal.trim(),
      window: windowKind,
    };
    const su = Number.parseFloat(softUsd);
    const hu = Number.parseFloat(hardUsd);
    const st = Number.parseInt(softTokens, 10);
    const ht = Number.parseInt(hardTokens, 10);
    if (softUsd.trim() && Number.isFinite(su)) body.soft_usd = su;
    if (hardUsd.trim() && Number.isFinite(hu)) body.hard_usd = hu;
    if (softTokens.trim() && Number.isFinite(st)) body.soft_tokens = st;
    if (hardTokens.trim() && Number.isFinite(ht)) body.hard_tokens = ht;
    if (!body.principal_id) {
      setFormError("Principal is required (user email or API key id).");
      return;
    }
    if (
      body.soft_usd === undefined &&
      body.hard_usd === undefined &&
      body.soft_tokens === undefined &&
      body.hard_tokens === undefined
    ) {
      setFormError("Set at least one bound (USD or tokens, soft or hard).");
      return;
    }
    setSaving(true);
    try {
      await orchestratorApi.createBudget(body);
      setPrincipal("");
      setSoftUsd("");
      setHardUsd("");
      setSoftTokens("");
      setHardTokens("");
      await reload();
    } catch (err) {
      setFormError(errorMessage(err, "Failed to save budget"));
    } finally {
      setSaving(false);
    }
  };

  const remove = async (id: string) => {
    if (deleting) return;
    setFormError(null);
    setDeleting(id);
    try {
      await orchestratorApi.deleteBudget(id);
      await reload();
    } catch (err) {
      setFormError(errorMessage(err, "Failed to delete budget"));
    } finally {
      setDeleting(null);
    }
  };

  if (error) {
    return <div className="table-error">Failed to load budgets: {error}</div>;
  }
  const budgets = data?.budgets ?? [];
  return (
    <div data-testid="budgets-section">
      <h3 className="usage-subheading">Budgets</h3>
      {budgets.length === 0 ? (
        <p className="usage-note" data-testid="budgets-empty">
          No budgets configured. A per-principal rolling budget (soft alert / hard
          refusal per day, week, or month) refuses new task creation when the
          window is exhausted.
        </p>
      ) : (
        <div className="table-wrapper">
          <table data-testid="budgets-table">
            <thead>
              <tr>
                <th scope="col">Scope</th>
                <th scope="col">Principal</th>
                <th scope="col">Window</th>
                <th scope="col">Spend</th>
                <th scope="col">Soft / hard (USD)</th>
                <th scope="col">Soft / hard (tokens)</th>
                <th scope="col">Status</th>
                <th scope="col">Actions</th>
              </tr>
            </thead>
            <tbody>
              {budgets.map((b) => (
                <tr key={b.id} data-testid="budget-row">
                  <td>{b.scope}</td>
                  <td className="prompt-cell" title={b.principal_id}>
                    {b.principal_id}
                  </td>
                  <td>{b.window}</td>
                  <td>
                    {fmtUSD(b.spend_usd)} · {compact.format(b.spend_tokens)} tok
                  </td>
                  <td>{budgetBound(b.soft_usd, b.hard_usd, b.effective_hard_usd, fmtUSD)}</td>
                  <td>
                    {budgetBound(b.soft_tokens, b.hard_tokens, b.effective_hard_tokens, (v) =>
                      compact.format(v),
                    )}
                  </td>
                  <td>{budgetStateLabel(b)}</td>
                  <td>
                    <button
                      type="button"
                      className="btn btn-secondary"
                      data-testid="budget-delete"
                      disabled={deleting === b.id}
                      onClick={() => void remove(b.id)}
                    >
                      {deleting === b.id ? "Deleting…" : "Delete"}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <form
        className="budget-form"
        data-testid="budget-create-form"
        onSubmit={(e) => {
          e.preventDefault();
          void submit();
        }}
      >
        <div className="task-eyebrow">New budget</div>
        <div className="budget-form-grid">
          <label htmlFor="budgetScope">Scope</label>
          <select
            id="budgetScope"
            aria-label="Budget scope"
            value={scope}
            onChange={(e) => setScope(e.target.value as BudgetCreate["scope"])}
          >
            <option value="user">User</option>
            <option value="key">API key</option>
          </select>
          <label htmlFor="budgetPrincipal">Principal</label>
          <input
            id="budgetPrincipal"
            type="text"
            aria-label="Budget principal"
            placeholder={scope === "user" ? "alice@example.com" : "key id"}
            value={principal}
            onChange={(e) => setPrincipal(e.target.value)}
          />
          <label htmlFor="budgetWindow">Window</label>
          <select
            id="budgetWindow"
            aria-label="Budget window"
            value={windowKind}
            onChange={(e) => setWindowKind(e.target.value as BudgetCreate["window"])}
          >
            <option value="day">Day</option>
            <option value="week">Week</option>
            <option value="month">Month</option>
          </select>
          <label htmlFor="budgetSoftUsd">Soft USD</label>
          <input
            id="budgetSoftUsd"
            type="number"
            min={0}
            step="0.01"
            inputMode="decimal"
            aria-label="Soft USD bound"
            placeholder="optional"
            value={softUsd}
            onChange={(e) => setSoftUsd(e.target.value)}
          />
          <label htmlFor="budgetHardUsd">Hard USD</label>
          <input
            id="budgetHardUsd"
            type="number"
            min={0}
            step="0.01"
            inputMode="decimal"
            aria-label="Hard USD bound"
            placeholder="optional"
            value={hardUsd}
            onChange={(e) => setHardUsd(e.target.value)}
          />
          <label htmlFor="budgetSoftTokens">Soft tokens</label>
          <input
            id="budgetSoftTokens"
            type="number"
            min={0}
            step={1}
            inputMode="numeric"
            aria-label="Soft token bound"
            placeholder="optional"
            value={softTokens}
            onChange={(e) => setSoftTokens(e.target.value)}
          />
          <label htmlFor="budgetHardTokens">Hard tokens</label>
          <input
            id="budgetHardTokens"
            type="number"
            min={0}
            step={1}
            inputMode="numeric"
            aria-label="Hard token bound"
            placeholder="optional"
            value={hardTokens}
            onChange={(e) => setHardTokens(e.target.value)}
          />
        </div>
        {formError ? (
          <div className="validation-error" role="alert" data-testid="budget-form-error">
            {formError}
          </div>
        ) : null}
        <button type="submit" className="btn btn-primary" disabled={saving}>
          {saving ? "Saving…" : "Save budget"}
        </button>
      </form>
      <p className="usage-note">
        A soft bound fires one alert per window; a hard bound refuses new task creation until
        the window rolls over. Hard bounds never exceed the live global ceilings. Dollar bounds
        cover only priced runs (see the note above) — zero-rated / included models (free: true or
        $0 pricing override) accrue $0.00 toward USD bounds while counting 100% of tokens toward
        token bounds.
      </p>
    </div>
  );
}

// budgetBound renders "soft / hard" with unset bounds as an em dash, and shows
// the effective (globally-clamped) hard value when it differs from the row's.
function budgetBound(
  soft: number | undefined,
  hard: number | undefined,
  effectiveHard: number | undefined,
  fmt: (v: number) => string,
): string {
  const softStr = soft === undefined ? "—" : fmt(soft);
  let hardStr = hard === undefined ? "—" : fmt(hard);
  if (hard !== undefined && effectiveHard !== undefined && effectiveHard < hard) {
    hardStr = `${fmt(effectiveHard)} (clamped from ${fmt(hard)})`;
  }
  return `${softStr} / ${hardStr}`;
}

// budgetStateLabel summarizes where the window stands: exhausted (a hard bound
// reached), alerted (this window's soft alert fired), or ok.
function budgetStateLabel(b: BudgetStatus): string {
  const hardUSD = b.effective_hard_usd ?? b.hard_usd;
  const hardTok = b.effective_hard_tokens ?? b.hard_tokens;
  if (
    (hardUSD !== undefined && b.spend_usd >= hardUSD) ||
    (hardTok !== undefined && b.spend_tokens >= hardTok)
  ) {
    return "exhausted";
  }
  return b.soft_alerted ? "alerted" : "ok";
}

type UsageMeasure = "cost" | "tokens" | "cache";

function bucketChatPromptTokens(b: UsageBucket): number {
  if (typeof b.chat_prompt_tokens === "number") {
    return b.chat_prompt_tokens;
  }
  if (b.chat_turns === 0 && b.task_iterations > 0) {
    return 0;
  }
  return b.prompt_tokens;
}

function bucketUncachedTokens(b: UsageBucket): number {
  if (typeof b.uncached_tokens === "number") {
    return b.uncached_tokens;
  }
  return Math.max(0, bucketChatPromptTokens(b) - b.cached_tokens);
}

function bucketCacheHitPct(b: UsageBucket): number {
  if (typeof b.cache_hit_rate_pct === "number") {
    return b.cache_hit_rate_pct;
  }
  const denom = bucketChatPromptTokens(b);
  return denom > 0 ? (b.cached_tokens / denom) * 100 : 0;
}

// UsageTotals — the KPI row: combined spend (with the per-source split as the
// sub-line) and the token totals that stay meaningful when pricing isn't
// configured.
function UsageTotals({ report }: { report: UsageReport }) {
  const t = report.totals;
  const hitPct = bucketCacheHitPct(t);
  const uncached = bucketUncachedTokens(t);
  return (
    <div className="usage-tiles" data-testid="usage-totals">
      <div className="usage-tile">
        <span className="usage-tile-label">Total cost</span>
        <span className="usage-tile-value">{fmtUSD(t.cost_usd)}</span>
        <span className="usage-tile-sub">
          tasks {fmtUSD(t.task_cost_usd)} · chat {fmtUSD(t.chat_cost_usd)}
        </span>
      </div>
      <div className="usage-tile">
        <span className="usage-tile-label">Prompt tokens</span>
        <span className="usage-tile-value">{compact.format(t.prompt_tokens)}</span>
        <span className="usage-tile-sub">{compact.format(t.cached_tokens)} cached (chat)</span>
      </div>
      <div className="usage-tile" data-testid="usage-cache-tile">
        <span className="usage-tile-label">Cache efficiency</span>
        <span className="usage-tile-value">{hitPct.toFixed(1)}%</span>
        <span className="usage-tile-sub">
          {compact.format(t.cached_tokens)} hit · {compact.format(uncached)} miss
        </span>
      </div>
      <div className="usage-tile">
        <span className="usage-tile-label">Completion tokens</span>
        <span className="usage-tile-value">{compact.format(t.completion_tokens)}</span>
      </div>
      <div className="usage-tile">
        <span className="usage-tile-label">Work units</span>
        <span className="usage-tile-value">{compact.format(t.task_iterations + t.chat_turns)}</span>
        <span className="usage-tile-sub">
          {compact.format(t.task_iterations)} task iterations · {compact.format(t.chat_turns)} chat
          turns
        </span>
      </div>
    </div>
  );
}

function measureOf(b: UsageBucket, measure: UsageMeasure): number {
  if (measure === "cost") return b.cost_usd;
  if (measure === "cache") return bucketCacheHitPct(b);
  return b.prompt_tokens + b.completion_tokens;
}

function fmtMeasure(v: number, measure: UsageMeasure): string {
  if (measure === "cost") return fmtUSD(v);
  if (measure === "cache") return `${v.toFixed(1)}%`;
  return compact.format(v);
}

function UsageChart({ report, measure }: { report: UsageReport; measure: UsageMeasure }) {
  const timeSeries = report.group_by === "day" || report.group_by === "week";
  return timeSeries ? (
    <UsageColumns report={report} measure={measure} />
  ) : (
    <UsageBars report={report} measure={measure} />
  );
}

// UsageBars — horizontal bars for the entity groupings (user/key/project/
// model). One hue; the category label wears text tokens beside the mark, the
// value is direct-labeled at the bar tip. Buckets past MAX_BARS fold into
// "Other" (the table below still lists every bucket).
function UsageBars({ report, measure }: { report: UsageReport; measure: UsageMeasure }) {
  const sorted = [...report.buckets].sort((a, b) => measureOf(b, measure) - measureOf(a, measure));
  const shown = sorted.slice(0, MAX_BARS);
  const tail = sorted.slice(MAX_BARS);
  const rows = shown.map((b) => ({
    name: bucketDisplayName(b, report.group_by),
    value: measureOf(b, measure),
  }));
  if (tail.length > 0) {
    let otherValue = 0;
    if (measure === "cache") {
      const tailCached = tail.reduce((sum, b) => sum + b.cached_tokens, 0);
      const tailPrompt = tail.reduce((sum, b) => sum + bucketChatPromptTokens(b), 0);
      otherValue = tailPrompt > 0 ? Math.min(100, (tailCached / tailPrompt) * 100) : 0;
    } else {
      otherValue = tail.reduce((sum, b) => sum + measureOf(b, measure), 0);
    }
    rows.push({
      name: `Other (${tail.length})`,
      value: otherValue,
    });
  }
  const max = measure === "cache" ? 100 : Math.max(...rows.map((r) => r.value), 0) || 1;
  return (
    <div className="usage-bars" data-testid="usage-bars">
      {rows.map((r) => (
        <div
          key={r.name}
          className="usage-bar-row"
          title={`${r.name}: ${fmtMeasure(r.value, measure)}`}
        >
          <span className="usage-bar-name" title={r.name}>
            {r.name}
          </span>
          <svg
            className="usage-bar-svg"
            role="img"
            aria-label={`${r.name}: ${fmtMeasure(r.value, measure)}`}
            viewBox="0 0 100 16"
            preserveAspectRatio="none"
          >
            {/* Square at the baseline, 4px-rounded data end. viewBox is
                non-uniformly scaled, so the rounding is applied via CSS
                (clip-path) rather than path arcs. */}
            <rect
              x="0"
              y="2"
              width={Math.max((r.value / max) * 100, r.value > 0 ? 0.75 : 0)}
              height="12"
              className="usage-bar-fill"
            />
          </svg>
          <span className="usage-bar-value">{fmtMeasure(r.value, measure)}</span>
        </div>
      ))}
    </div>
  );
}

// UsageColumns — the day/week time series as thin columns from a zero
// baseline, with three clean-ish y gridlines and first/last date labels. Every
// column carries a native tooltip; the table below carries every value.
function UsageColumns({ report, measure }: { report: UsageReport; measure: UsageMeasure }) {
  const buckets = report.buckets; // already sorted chronologically server-side
  const W = 720;
  const H = 160;
  const padTop = 8;
  const padBottom = 20;
  const plotH = H - padTop - padBottom;
  const max = measure === "cache" ? 100 : Math.max(...buckets.map((b) => measureOf(b, measure)), 0) || 1;
  const band = W / buckets.length;
  const barW = Math.min(Math.max(band - 2, 1), 24); // 2px surface gap, ≤24px thick
  const y = (v: number) => padTop + plotH - (v / max) * plotH;
  const gridValues = [max, max / 2];
  const measureLabel = measure === "cost" ? "Cost" : measure === "cache" ? "Cache hit %" : "Tokens";
  return (
    <div className="usage-columns-wrap">
      <svg
        className="usage-columns"
        role="img"
        aria-label={`${measureLabel} per ${report.group_by}, ${buckets.length} buckets`}
        viewBox={`0 0 ${W} ${H}`}
      >
        {gridValues.map((v) => (
          <g key={v}>
            <line x1={0} y1={y(v)} x2={W} y2={y(v)} className="usage-grid" />
            <text x={W - 2} y={y(v) - 3} textAnchor="end" className="usage-axis-text">
              {fmtMeasure(v, measure)}
            </text>
          </g>
        ))}
        <line x1={0} y1={y(0)} x2={W} y2={y(0)} className="usage-grid usage-grid-baseline" />
        {buckets.map((b, i) => {
          const v = measureOf(b, measure);
          const h = Math.max(((v / max) * plotH), v > 0 ? 1 : 0);
          return (
            <rect
              key={b.key}
              x={i * band + (band - barW) / 2}
              y={y(0) - h}
              width={barW}
              height={h}
              rx={Math.min(4, barW / 2, h)}
              className="usage-bar-fill"
            >
              <title>{`${b.key}: ${fmtMeasure(v, measure)}`}</title>
            </rect>
          );
        })}
        <text x={2} y={H - 6} className="usage-axis-text">
          {buckets[0].key}
        </text>
        {buckets.length > 1 ? (
          <text x={W - 2} y={H - 6} textAnchor="end" className="usage-axis-text">
            {buckets[buckets.length - 1].key}
          </text>
        ) : null}
      </svg>
    </div>
  );
}

// UsageTable — the WCAG-clean table twin: every bucket, every column, no
// hover required.
function UsageTable({ report }: { report: UsageReport }) {
  const groupLabel = GROUP_OPTIONS.find((o) => o.value === report.group_by)?.label ?? "Bucket";
  return (
    <div className="table-wrapper">
      <table data-testid="usage-table">
        <thead>
          <tr>
            <th scope="col">{groupLabel}</th>
            <th scope="col">Cost</th>
            <th scope="col">Task cost</th>
            <th scope="col">Chat cost</th>
            <th scope="col">Prompt tokens</th>
            <th scope="col">Completion tokens</th>
            <th scope="col">Cached (chat)</th>
            <th scope="col">Cache miss</th>
            <th scope="col">Cache hit %</th>
            <th scope="col">Iterations</th>
            <th scope="col">Turns</th>
          </tr>
        </thead>
        <tbody>
          {report.buckets.map((b) => (
            <tr key={b.key} data-testid="usage-row">
              <td className="prompt-cell" title={bucketDisplayName(b, report.group_by)}>
                {bucketDisplayName(b, report.group_by)}
              </td>
              <td>{fmtUSD(b.cost_usd)}</td>
              <td>{fmtUSD(b.task_cost_usd)}</td>
              <td>{fmtUSD(b.chat_cost_usd)}</td>
              <td className="usage-num">{plain.format(b.prompt_tokens)}</td>
              <td className="usage-num">{plain.format(b.completion_tokens)}</td>
              <td className="usage-num">{plain.format(b.cached_tokens)}</td>
              <td className="usage-num">{plain.format(bucketUncachedTokens(b))}</td>
              <td className="usage-num">{bucketCacheHitPct(b).toFixed(1)}%</td>
              <td className="usage-num">{plain.format(b.task_iterations)}</td>
              <td className="usage-num">{plain.format(b.chat_turns)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export default UsagePanel;
