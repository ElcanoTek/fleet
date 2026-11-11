import { formatFixed } from "@/app/lib/i18n";
import { localizeServerMessage } from "@/app/lib/i18n/errors";
import { rich } from "@/app/lib/i18n/rich";
import { getLanguage } from "@/app/lib/i18n";
import { t as tr } from "@/app/lib/i18n";
import type { CostForecast } from "@/app/shared/lib/orchestratorApi";

// CostForecastPanel renders the pre-submission cost forecast inline below the
// Create Task form (#233). It always shows the token breakdown; it shows the
// dollar estimate + range only when the model's pricing is known, and a warning
// banner when the median would exceed the configured cost ceiling. Advisory
// only — it never gates Submit.
export function CostForecastPanel({ forecast }: { forecast: CostForecast }) {
  const usd = (v: number) => `$${formatFixed(v, 4)}`;
  return (
    <div className="cost-forecast" role="status" aria-live="polite">
      {forecast.would_hit_ceiling ? (
        <div className="cost-forecast-warning" role="alert">
        {rich("Estimated cost (~{0}) exceeds the cost ceiling of ${1} — the run may stop early.", usd(forecast.estimated_total_cost_usd ?? 0), formatFixed(forecast.max_cost_ceiling_usd, 2))}
      </div>
      ) : null}
      <dl className="cost-forecast-grid">
        <dt>{tr("Model")}</dt>
        <dd>{forecast.model}</dd>
        <dt>{tr("Estimated prompt")}</dt>
        <dd>
          {tr("{0} tokens (system {1} + tools {2})", [forecast.estimated_prompt_tokens.toLocaleString(getLanguage()), forecast.system_prompt_tokens.toLocaleString(getLanguage()), forecast.tool_definitions_tokens.toLocaleString(getLanguage())])}
        </dd>
        <dt>{tr("Max iterations")}</dt>
        <dd>{forecast.max_iterations}</dd>
        {forecast.pricing_known &&
        forecast.estimated_total_cost_usd != null &&
        forecast.estimated_total_cost_range != null ? (
          <>
            <dt>{tr("Estimated cost")}</dt>
            <dd>
              {tr("{0} (range {1} – {2})", [usd(forecast.estimated_total_cost_usd), usd(forecast.estimated_total_cost_range.min), usd(forecast.estimated_total_cost_range.max)])}
            </dd>
          </>
        ) : null}
      </dl>
      <p className="cost-forecast-note">{localizeServerMessage(forecast.note)}</p>
    </div>
  );
}

export default CostForecastPanel;
