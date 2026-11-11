import { formatFixed } from "@/app/lib/i18n";
import { rich } from "@/app/lib/i18n/rich";
import { t as tr } from "@/app/lib/i18n";
import { useCallback, useState } from "react";
import { orchestratorApi, type SLAReport } from "@/app/shared/lib/orchestratorApi";
import { useCancellableFetch } from "@/app/shared/hooks/useCancellableFetch";
import { plural } from "@/app/shared/lib/format";
import { SlaChip } from "./taskDisplay";

// SLAReportPanel — the Operations Center SLA tab (#274): a per-task-name table
// of the actual-duration p50/p95 + breach rate over a window, plus a tiny SVG
// sparkline per row visualizing actual vs. expected. Driven by GET /sla-report
// (admin-only). The window defaults to 7 days; the operator can widen it to 30.

const WINDOW_OPTIONS = [7, 14, 30];

export function SLAReportPanel() {
  const [days, setDays] = useState(7);
  const {
    data: report,
    loading,
    error,
    reload,
  } = useCancellableFetch(
    useCallback((signal: AbortSignal) => orchestratorApi.slaReport(days, signal), [days]),
    [days],
  );

  return (
    <div className="section" role="region" aria-labelledby="slaHeading">
      <div className="section-header">
        <h2 id="slaHeading">{tr("SLA Report")}</h2>
        {/* No auto-refresh on this tab (only the task list polls), so the
            report is as fresh as the last click here or a window change. */}
        <button
          type="button"
          className="btn btn-secondary btn-small"
          aria-label={tr("Refresh SLA report")}
          data-testid="sla-refresh"
          disabled={loading}
          onClick={() => void reload()}
        >
          {tr("Refresh")}
        </button>
        <div className="sla-window-select">
          <label htmlFor="slaWindow">{tr("Window")}</label>
          <select
            id="slaWindow"
            aria-label={tr("SLA report window in days")}
            value={days}
            onChange={(e) => setDays(Number.parseInt(e.target.value, 10))}
          >
            {WINDOW_OPTIONS.map((d) => (
              <option key={d} value={d}>
                {tr("Last")}{" "}{d} {tr("days")}
              </option>
            ))}
          </select>
        </div>
      </div>

      {loading ? (
        <div className="loading">
          <p>{tr("Loading SLA report…")}</p>
        </div>
      ) : error ? (
        <div className="table-error">
        {rich("Failed to load SLA report: {0}", error)}
      </div>
      ) : !report || report.tasks.length === 0 ? (
        <div className="table-empty">{tr("No SLA-tracked task runs in this window.")}</div>
      ) : (
        <SLATable report={report} />
      )}
    </div>
  );
}

function SLATable({ report }: { report: SLAReport }) {
  return (
    <div className="table-wrapper">
      <table id="slaReportTable" data-testid="sla-report-table">
        <thead>
          <tr>
            <th scope="col">{tr("Task")}</th>
            <th scope="col">{tr("Expected")}</th>
            <th scope="col">{tr("p50 actual")}</th>
            <th scope="col">{tr("p95 actual")}</th>
            <th scope="col">{tr("Breach rate")}</th>
            <th scope="col">{tr("Runs")}</th>
            <th scope="col">{tr("Distribution")}</th>
          </tr>
        </thead>
        <tbody>
          {report.tasks.map((row) => {
            const breachTone =
              row.breach_rate_pct >= 50 ? "fail" : row.breach_rate_pct > 0 ? "warn" : "ok";
            return (
              <tr key={row.task_name} data-testid="sla-report-row">
                <td className="prompt-cell" title={row.task_name}>{row.task_name}</td>
                <td>{row.expected_minutes}{tr("m")}</td>
                <td>{formatFixed(row.p50_actual_minutes, 1)}{tr("m")}</td>
                <td>{formatFixed(row.p95_actual_minutes, 1)}{tr("m")}</td>
                <td>
                  <SlaChip tone={breachTone}>{formatFixed(row.breach_rate_pct, 1)}%</SlaChip>
                </td>
                <td>{row.sample_count}</td>
                <td>
                  <SLASparkline
                    expected={row.expected_minutes}
                    p50={row.p50_actual_minutes}
                    p95={row.p95_actual_minutes}
                    tone={breachTone}
                  />
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      <p className="refresh-note">
        {rich("Window: {0} · {1}", report.period, plural(report.tasks.length, "task bucket"))}
      </p>
    </div>
  );
}

// SLASparkline is a tiny inline SVG that draws the expected duration as a
// vertical reference line and the p50→p95 band as a horizontal bar, capped at
// 2× expected so a runaway p95 doesn't crush the scale. tone drives the bar
// color (ok/warn/fail), matching the badge palette.
function SLASparkline({
  expected,
  p50,
  p95,
  tone,
}: {
  expected: number;
  p50: number;
  p95: number;
  tone: string;
}) {
  const W = 120;
  const H = 24;
  const scale = (v: number) => {
    const max = expected * 2 || 1;
    return Math.min(v / max, 1) * (W - 4) + 2;
  };
  const xExp = scale(expected);
  const x50 = scale(p50);
  const x95 = scale(p95);
  return (
    <svg
      width={W}
      height={H}
      role="img"
      aria-label={tr("p50 {0}m, p95 {1}m, expected {2}m", [formatFixed(p50, 1), formatFixed(p95, 1), expected])}
    >
      <rect x={x50} y={6} width={Math.max(x95 - x50, 1)} height={12} className={`sla-spark sla-spark-${tone}`} />
      <line x1={xExp} y1={2} x2={xExp} y2={H - 2} className="sla-spark-expected" />
      <circle cx={x50} cy={12} r={2.5} className="sla-spark-dot" />
    </svg>
  );
}

export default SLAReportPanel;
