"use client";

import type { CostForecast } from "@/app/shared/lib/orchestratorApi";

// CostForecastPanel renders the pre-submission cost forecast inline below the
// Create Task form (#233). It always shows the token breakdown; it shows the
// dollar estimate + range only when the model's pricing is known, and a warning
// banner when the median would exceed the configured cost ceiling. Advisory
// only — it never gates Submit.
export function CostForecastPanel({ forecast }: { forecast: CostForecast }) {
  const usd = (v: number) => `$${v.toFixed(4)}`;
  return (
    <div className="cost-forecast" role="status" aria-live="polite">
      {forecast.would_hit_ceiling ? (
        <div className="cost-forecast-warning" role="alert">
          Estimated cost (~{usd(forecast.estimated_total_cost_usd ?? 0)}) exceeds the cost ceiling of $
          {forecast.max_cost_ceiling_usd.toFixed(2)} — the run may stop early.
        </div>
      ) : null}
      <dl className="cost-forecast-grid">
        <dt>Model</dt>
        <dd>{forecast.model}</dd>
        <dt>Estimated prompt</dt>
        <dd>
          {forecast.estimated_prompt_tokens.toLocaleString()} tokens (system{" "}
          {forecast.system_prompt_tokens.toLocaleString()} + tools{" "}
          {forecast.tool_definitions_tokens.toLocaleString()})
        </dd>
        <dt>Max iterations</dt>
        <dd>{forecast.max_iterations}</dd>
        {forecast.pricing_known &&
        forecast.estimated_total_cost_usd != null &&
        forecast.estimated_total_cost_range != null ? (
          <>
            <dt>Estimated cost</dt>
            <dd>
              {usd(forecast.estimated_total_cost_usd)} (range {usd(forecast.estimated_total_cost_range.min)} –{" "}
              {usd(forecast.estimated_total_cost_range.max)})
            </dd>
          </>
        ) : null}
      </dl>
      <p className="cost-forecast-note">{forecast.note}</p>
    </div>
  );
}

export default CostForecastPanel;
