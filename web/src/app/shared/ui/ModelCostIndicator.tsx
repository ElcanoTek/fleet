import { modelCostFor, MAX_COST_TIER, type ModelPrices } from "@/app/shared/lib/modelCost";

// ModelCostIndicator — the restaurant-style "$ … $$$$" price glyphs shown next
// to a model in both pickers (chat composer listbox and the orchestrator task
// form). Filled glyphs carry the tier; the remaining glyphs stay as a dimmed
// track so "$$" reads as two-of-four rather than as an unqualified "$$".
//
// Renders null when the model has no known pricing (workspace-provider models,
// free-typed slugs) — an absent indicator is honest, a guessed one is not.
//
// Styling lives in globals.css (.model-cost*) so the same component drops into
// the Tailwind-authored chat composer and the CSS-class-authored orchestrator
// form without a variant flag.

export function ModelCostIndicator({
  prices,
  className,
}: {
  prices: ModelPrices | null | undefined;
  className?: string;
}) {
  const cost = modelCostFor(prices);
  if (!cost) return null;
  const rest = MAX_COST_TIER - cost.tier;
  return (
    <span
      className={`model-cost model-cost-tier-${cost.tier}${className ? ` ${className}` : ""}`}
      title={cost.description}
      aria-label={cost.description}
      data-cost-tier={cost.tier}
    >
      <span className="model-cost-filled" aria-hidden="true">
        {cost.symbol}
      </span>
      {rest > 0 ? (
        <span className="model-cost-rest" aria-hidden="true">
          {"$".repeat(rest)}
        </span>
      ) : null}
    </span>
  );
}

export default ModelCostIndicator;
