import { afterEach, describe, expect, it } from "vitest";
import { render, screen, cleanup } from "@testing-library/react";
import { CostForecastPanel } from "./CostForecastPanel";
import type { CostForecast } from "@/app/shared/lib/orchestratorApi";

const baseForecast: CostForecast = {
  model: "anthropic/claude-sonnet-4-5",
  estimated_prompt_tokens: 12500,
  system_prompt_tokens: 8200,
  tool_definitions_tokens: 3100,
  avg_output_tokens: 800,
  max_iterations: 20,
  pricing_known: true,
  per_iteration_cost_usd: 0.018,
  estimated_total_cost_usd: 0.36,
  estimated_total_cost_range: { min: 0.09, max: 0.36 },
  max_cost_ceiling_usd: 1.0,
  would_hit_ceiling: false,
  note: "Range is 0.25x-1x the median; actual cost depends on task complexity.",
};

describe("CostForecastPanel", () => {
  afterEach(() => cleanup());

  it("shows the token breakdown and dollar estimate for a known model", () => {
    render(<CostForecastPanel forecast={baseForecast} />);
    expect(screen.getByText("anthropic/claude-sonnet-4-5")).toBeInTheDocument();
    expect(screen.getByText(/12,500 tokens/)).toBeInTheDocument();
    expect(screen.getByText(/\$0\.3600/)).toBeInTheDocument();
    // No ceiling warning when within budget.
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("renders a warning banner when the estimate would hit the ceiling", () => {
    render(
      <CostForecastPanel
        forecast={{ ...baseForecast, estimated_total_cost_usd: 5.0, would_hit_ceiling: true }}
      />,
    );
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent(/exceeds the cost ceiling/);
  });

  it("omits the dollar estimate when the model's pricing is unknown", () => {
    render(
      <CostForecastPanel
        forecast={{
          ...baseForecast,
          model: "vendor/unknown",
          pricing_known: false,
          per_iteration_cost_usd: null,
          estimated_total_cost_usd: null,
          estimated_total_cost_range: null,
          note: "pricing for model vendor/unknown is unknown; cost fields are null.",
        }}
      />,
    );
    // Token breakdown still present.
    expect(screen.getByText(/12,500 tokens/)).toBeInTheDocument();
    // No "Estimated cost" row when pricing is unknown.
    expect(screen.queryByText("Estimated cost")).not.toBeInTheDocument();
    expect(screen.getByText(/pricing for model vendor\/unknown is unknown/)).toBeInTheDocument();
  });
});
