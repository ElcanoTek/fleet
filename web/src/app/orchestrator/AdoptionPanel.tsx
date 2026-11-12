import { rich } from "@/app/lib/i18n/rich";
import { getLanguage } from "@/app/lib/i18n";
import { t as tr } from "@/app/lib/i18n";
import { useCallback, useState } from "react";
import { orchestratorApi, type AdoptionReport, type AdoptionUser } from "@/app/shared/lib/orchestratorApi";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";
import { downloadFile } from "@/app/shared/lib/downloadFile";
import { errorMessage } from "@/app/shared/lib/errorMessage";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

// AdoptionPanel — the Operations Center's executive Adoption view: who is
// actually using the agents, how often, and is that growing. Driven by
// GET /admin/usage/adoption (admin-only), which merges both meters per user ×
// UTC day and carries an equal-length previous window for the trend deltas.
//
// Dataviz notes: adoption is read on TOKEN volume (the coverage-independent
// meter, #289 — unpriced runs cost $0 but still count tokens), so tokens lead
// everywhere and dollars ride along. All marks are single-measure magnitude
// and wear the one validated hue (--usage-bar); the two daily trends are
// small multiples sharing one x-axis — never a dual-axis chart. Deltas pair
// an arrow glyph with the color so direction is never color-alone, and the
// leaderboard table is the WCAG-clean twin carrying every value the
// sparklines only sketch.
//
// Honest scope: the server note (rendered verbatim) carries both caveats —
// dollar coverage depends on pricing config, and token volume measures
// activity, not output quality.

const RANGE_OPTIONS = [7, 30, 90] as const;

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
const compact = new Intl.NumberFormat(getLanguage(), { notation: "compact", maximumFractionDigits: 1 });
const plain = new Intl.NumberFormat(getLanguage());

function fmtUSD(v: number): string {
  return v > 0 && v < 0.01 ? usdFine.format(v) : usd.format(v);
}

function tokensOf(u: AdoptionUser): number {
  return u.prompt_tokens + u.completion_tokens;
}

// engagementTier buckets a user by how much of the window they were active:
// a habit signal that raw volume can't carry (one huge batch day ≠ adoption).
function engagementTier(activeDays: number, windowDays: number): "power" | "regular" | "light" {
  if (windowDays <= 0) return "light";
  const share = activeDays / windowDays;
  if (share >= 0.5) return "power";
  if (share >= 0.2) return "regular";
  return "light";
}

// DeltaChip — change vs the previous equal-length window. The arrow glyph
// carries direction alongside the color (never color-alone); "new" marks
// activity with no previous-window baseline to compare against.
function DeltaChip({ cur, prev, what }: { cur: number; prev: number; what: string }) {
  if (prev <= 0 && cur <= 0) return null;
  if (prev <= 0) {
    return (
      <span className="adoption-delta adoption-delta-new" title={tr("No {0} in the previous period", [what])}>
        {tr("new")}
      </span>
    );
  }
  const pct = Math.round(((cur - prev) / prev) * 100);
  if (pct === 0) {
    return (
      <span className="adoption-delta" title={tr("Unchanged vs the previous period")}>
        ±0%
      </span>
    );
  }
  const up = pct > 0;
  return (
    <span
      className={`adoption-delta ${up ? "adoption-delta-up" : "adoption-delta-down"}`}
      title={tr("{0} {1}% vs the previous period ({2})", [up ? tr("Up") : tr("Down"), Math.abs(pct), what])}
    >
      {up ? "▲" : "▼"} {Math.abs(pct)}%
    </span>
  );
}

export function AdoptionPanel() {
  const [rangeDays, setRangeDays] = useState<number>(30);
  // A failed CSV download is shown in place (the button used to navigate).
  const [downloadError, setDownloadError] = useState<string | null>(null);

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
      return orchestratorApi.adoption({ from }, signal);
    }, [rangeDays]),
    [rangeDays],
  );

  return (
    <div className="section" role="region" aria-labelledby="adoptionHeading">
      <div className="section-header">
        <h2 id="adoptionHeading">{tr("AI Adoption")}</h2>
        <div className="usage-controls">
          <label htmlFor="adoptionRange">{tr("Range")}</label>
          <NativeSelect
            id="adoptionRange"
            className="w-auto"
            aria-label={tr("Adoption window in days")}
            value={rangeDays}
            onChange={(e) => setRangeDays(Number.parseInt(e.target.value, 10))}
          >
            {RANGE_OPTIONS.map((d) => (
              <NativeSelectOption key={d} value={d}>
                {tr("Last")}{" "}{d} {tr("days")}
              </NativeSelectOption>
            ))}
          </NativeSelect>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="btn"
            aria-label={tr("Download the per-user adoption report as CSV")}
            data-testid="adoption-download-csv"
            onClick={() => {
              const from = new Date(Date.now() - rangeDays * 24 * 60 * 60 * 1000)
                .toISOString()
                .slice(0, 10);
              const qs = new URLSearchParams({ format: "csv", from });
              // Fetch-then-save (downloadFile) instead of navigating: a 401/500
              // used to replace the dashboard with the server's error body.
              void downloadFile(`/api/orchestrator/admin/usage/adoption?${qs.toString()}`, `adoption-${from}.csv`).catch(
                (err: unknown) => setDownloadError(errorMessage(err, tr("download failed"))),
              );
            }}
          >
            {tr("Download CSV")}
          </Button>
        </div>
      </div>

      {downloadError ? (
        <div className="table-error" role="alert" data-testid="adoption-download-error">
        {rich("Couldn't download the CSV: {0}", downloadError)}
      </div>
      ) : null}
      {error ? (
        <div className="table-error">
        {rich("Failed to load adoption report: {0}", error)}
      </div>
      ) : !report && loading ? (
        <div className="loading">
          <p>{tr("Loading adoption report…")}</p>
        </div>
      ) : !report ? null : (
        <div className={loading ? "usage-refetching" : undefined}>
          <AdoptionTotalsTiles report={report} />
          <p className="usage-note" data-testid="adoption-note">
            {report.note}
          </p>
          {report.users.length === 0 ? (
            <div className="table-empty">{tr("No recorded activity in this window.")}</div>
          ) : (
            <>
              <AdoptionTrends report={report} />
              <AdoptionLeaderboard report={report} />
            </>
          )}
          <InactiveSeats report={report} />
          <p className="refresh-note">
        {rich("{0} → {1} · compared with {2} → {3} · sources: {4}", report.from.slice(0, 10), report.to.slice(0, 10), report.prev_from.slice(0, 10), report.from.slice(0, 10), report.sources.join(" + "))}
      </p>
        </div>
      )}
    </div>
  );
}

// AdoptionTotalsTiles — the KPI row an exec reads first: headcounts with
// their trend, then the volume/spend totals. Token volume leads (#289).
function AdoptionTotalsTiles({ report }: { report: AdoptionReport }) {
  const t = report.totals;
  const seatsKnown = report.sources.includes("accounts") && t.registered_users > 0;
  const adoptionPct = seatsKnown ? Math.round((t.active_users / t.registered_users) * 100) : null;
  return (
    <div className="usage-tiles" data-testid="adoption-totals">
      <Card className="usage-tile">
        <span className="usage-tile-label">{tr("Active users")}</span>
        <span className="usage-tile-value">
          {plain.format(t.active_users)}{" "}
          <DeltaChip cur={t.active_users} prev={t.prev_active_users} what="active users" />
        </span>
        <span className="usage-tile-sub">
          {seatsKnown
            ? tr("of {0} seats · {1}% adoption", [plain.format(t.registered_users), adoptionPct])
            : tr("seat roster unavailable")}
        </span>
      </Card>
      <Card className="usage-tile">
        <span className="usage-tile-label">{tr("New this period")}</span>
        <span className="usage-tile-value">{plain.format(t.new_active_users)}</span>
        <span className="usage-tile-sub">{tr("active now, not in the previous period")}</span>
      </Card>
      <Card className="usage-tile">
        <span className="usage-tile-label">{tr("Tokens")}</span>
        <span className="usage-tile-value">
          {compact.format(t.tokens)} <DeltaChip cur={t.tokens} prev={t.prev_tokens} what="tokens" />
        </span>
        <span className="usage-tile-sub">
        {rich("{0} cached (chat)", compact.format(t.cached_tokens))}
      </span>
      </Card>
      <Card className="usage-tile">
        <span className="usage-tile-label">{tr("Spend")}</span>
        <span className="usage-tile-value">
          {fmtUSD(t.cost_usd)} <DeltaChip cur={t.cost_usd} prev={t.prev_cost_usd} what="spend" />
        </span>
        <span className="usage-tile-sub">
        {rich("{0} task iterations · {1} chat turns", compact.format(t.task_iterations), compact.format(t.chat_turns))}
      </span>
      </Card>
    </div>
  );
}

// AdoptionTrends — two small multiples over the shared day axis: daily token
// volume and daily active users. Two measures of different scale, so two
// charts (one hue each), never a dual-axis overlay.
function AdoptionTrends({ report }: { report: AdoptionReport }) {
  return (
    <div className="adoption-trends">
      <TrendColumns
        title={tr("Tokens per day")}
        days={report.days}
        values={report.daily.map((d) => d.tokens)}
        fmt={(v) => compact.format(v)}
      />
      <TrendColumns
        title={tr("Active users per day")}
        days={report.days}
        values={report.daily.map((d) => d.active_users)}
        fmt={(v) => plain.format(v)}
      />
    </div>
  );
}

// TrendColumns — one daily magnitude series as thin columns from a zero
// baseline (the UsagePanel column recipe at small-multiple size): two
// gridlines, first/last date labels, native tooltips per column.
function TrendColumns({
  title,
  days,
  values,
  fmt,
}: {
  title: string;
  days: string[];
  values: number[];
  fmt: (v: number) => string;
}) {
  const W = 360;
  const H = 128;
  // Enough headroom that the top gridline's label renders inside the viewBox
  // instead of clipping against the chart top.
  const padTop = 16;
  const padBottom = 18;
  const plotH = H - padTop - padBottom;
  const max = Math.max(...values, 0) || 1;
  const band = W / Math.max(days.length, 1);
  const barW = Math.min(Math.max(band - 2, 1), 18);
  const y = (v: number) => padTop + plotH - (v / max) * plotH;
  // Integer measures (active users) get an integer mid gridline — "1.5 users"
  // is not a value this chart can show. Dedupe when rounding collapses them.
  const gridValues = [...new Set([max, Math.round(max / 2)])].filter((v) => v > 0);
  return (
    <div className="adoption-trend">
      <h3 className="usage-subheading">{title}</h3>
      <svg
        className="adoption-trend-svg"
        role="img"
        aria-label={tr("{0}, {1} days, peak {2}", [title, days.length, fmt(max)])}
        viewBox={`0 0 ${W} ${H}`}
      >
        {gridValues.map((v) => (
          <g key={v}>
            <line x1={0} y1={y(v)} x2={W} y2={y(v)} className="usage-grid" />
            <text x={W - 2} y={y(v) - 3} textAnchor="end" className="usage-axis-text">
              {fmt(v)}
            </text>
          </g>
        ))}
        <line x1={0} y1={y(0)} x2={W} y2={y(0)} className="usage-grid usage-grid-baseline" />
        {values.map((v, i) => {
          const h = Math.max((v / max) * plotH, v > 0 ? 1 : 0);
          return (
            <rect
              key={days[i]}
              x={i * band + (band - barW) / 2}
              y={y(0) - h}
              width={barW}
              height={h}
              rx={Math.min(4, barW / 2, h)}
              className="usage-bar-fill"
            >
              <title>{`${days[i]}: ${fmt(v)}`}</title>
            </rect>
          );
        })}
        <text x={2} y={H - 4} className="usage-axis-text">
          {days[0]}
        </text>
        {days.length > 1 ? (
          <text x={W - 2} y={H - 4} textAnchor="end" className="usage-axis-text">
            {days[days.length - 1]}
          </text>
        ) : null}
      </svg>
    </div>
  );
}

// Sparkline — one user's daily token series at glanceable size. Decorative
// trend only: the row's numeric columns carry every real value, and the
// aria-label summarizes it for screen readers.
function Sparkline({ user, days }: { user: AdoptionUser; days: string[] }) {
  const values = user.daily_tokens;
  const max = Math.max(...values, 0);
  if (max <= 0) return null;
  const W = values.length * 4;
  const H = 20;
  return (
    <svg
      className="adoption-spark"
      role="img"
      aria-label={tr("Daily tokens for {0}: active {1} of {2} days", [user.user || tr("(unattributed)"), user.active_days, days.length])}
      viewBox={`0 0 ${W} ${H}`}
      preserveAspectRatio="none"
    >
      {values.map((v, i) =>
        v > 0 ? (
          <rect
            key={days[i]}
            x={i * 4}
            y={H - Math.max((v / max) * H, 1.5)}
            width={3}
            height={Math.max((v / max) * H, 1.5)}
            className="usage-bar-fill"
          />
        ) : null,
      )}
    </svg>
  );
}

// AdoptionLeaderboard — the per-user audit table, token volume first. Every
// value the tiles/sparklines sketch is carried here in full.
function AdoptionLeaderboard({ report }: { report: AdoptionReport }) {
  const windowDays = report.days.length;
  return (
    <>
      <h3 className="usage-subheading">{tr("Usage by user")}</h3>
      <Table data-testid="adoption-table" containerClassName="table-wrapper">
        <TableHeader>
          <TableRow>
            <TableHead>#</TableHead>
            <TableHead>{tr("User")}</TableHead>
            <TableHead>{tr("Trend")}</TableHead>
            <TableHead>{tr("Tokens")}</TableHead>
            <TableHead>{tr("vs prev")}</TableHead>
            <TableHead>{tr("Active days")}</TableHead>
            <TableHead>{tr("Engagement")}</TableHead>
            <TableHead>{tr("Cost")}</TableHead>
            <TableHead>{tr("Chat turns")}</TableHead>
            <TableHead>{tr("Task iterations")}</TableHead>
            <TableHead>{tr("Last active")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {report.users.map((u, i) => {
            const tier = engagementTier(u.active_days, windowDays);
            return (
              <TableRow key={u.user || "(unattributed)"} data-testid="adoption-row">
                <TableCell className="usage-num">{i + 1}</TableCell>
                <TableCell className="prompt-cell whitespace-normal" title={u.user || tr("(unattributed)")}>
                  {u.user || tr("(unattributed)")}
                </TableCell>
                <TableCell>
                  <Sparkline user={u} days={report.days} />
                </TableCell>
                <TableCell className="usage-num">{plain.format(tokensOf(u))}</TableCell>
                <TableCell>
                  <DeltaChip cur={tokensOf(u)} prev={u.prev_tokens} what="tokens" />
                </TableCell>
                <TableCell className="usage-num">
                  {u.active_days}/{windowDays}
                </TableCell>
                <TableCell>
                  <span className={`adoption-tier adoption-tier-${tier}`}>{tier}</span>
                </TableCell>
                <TableCell>{fmtUSD(u.cost_usd)}</TableCell>
                <TableCell className="usage-num">{plain.format(u.chat_turns)}</TableCell>
                <TableCell className="usage-num">{plain.format(u.task_iterations)}</TableCell>
                <TableCell>{u.last_active ?? "—"}</TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
      <p className="usage-note">
        {tr("Engagement buckets by share of days active: power ≥ 50%, regular ≥ 20%, light below. Tokens count prompt + completion across chat and scheduled tasks.")}
      </p>
    </>
  );
}

// InactiveSeats — the other half of the adoption question: provisioned
// accounts with no metered activity in the window (churned or never started).
// Hidden when the account roster isn't wired (sources omits "accounts").
function InactiveSeats({ report }: { report: AdoptionReport }) {
  if (!report.sources.includes("accounts")) return null;
  const seats = report.inactive_users;
  return (
    <div data-testid="adoption-inactive">
      <h3 className="usage-subheading">
        {rich("Not yet active ({0})", seats.length)}
      </h3>
      {seats.length === 0 ? (
        <p className="usage-note">{tr("Every provisioned account was active in this window.")}</p>
      ) : (
        <ul className="adoption-seats">
          {seats.map((s) => (
            <li
              key={s.email}
              className="adoption-seat"
              title={tr("Provisioned {0}, no activity in this window", [s.created_at.slice(0, 10)])}
            >
              {s.email}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

export default AdoptionPanel;
