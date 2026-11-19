import { t as tr } from "@/app/lib/i18n";
import { modelCostFor, MAX_COST_TIER, type ModelPrices } from "@/app/shared/lib/modelCost";

// ModelCostIndicator — the credit-usage meter shown next to a model in both
// pickers (chat composer listbox and the orchestrator task form): four pips,
// of which the first N are filled for level N. Level is carried twice — by how
// many pips are filled and by their colour, which warms from green (low) to
// red (very high) — so it does not depend on colour vision. The exact blended
// price, in credits, is the title/accessible name.
//
// Renders null when the model has no known pricing (workspace-provider models,
// free-typed slugs) — an absent indicator is honest, a guessed one is not.
//
// Styling lives in globals.css (.model-cost*) so the same component drops into
// the Tailwind-authored chat composer and the CSS-class-authored orchestrator
// form without a variant flag.

const PIPS = Array.from({ length: MAX_COST_TIER }, (_, i) => i + 1);

export function ModelCostIndicator({
  prices,
  free,
  className,
}: {
  prices: ModelPrices | null | undefined;
  // An operator pin marked free (FLEET_PINNED_MODELS) reads "Free" instead of
  // a meter. Display only — the provider's real price is still metered.
  free?: boolean;
  className?: string;
}) {
  if (free) {
    return (
      <span
        className={`model-cost model-cost-free${className ? ` ${className}` : ""}`}
        title={tr("Free")}
        aria-label={tr("Free")}
        data-cost-tier="free"
      >
        {tr("Free")}
      </span>
    );
  }
  const cost = modelCostFor(prices);
  if (!cost) return null;
  return (
    <span
      className={`model-cost model-cost-tier-${cost.tier}${className ? ` ${className}` : ""}`}
      role="img"
      title={cost.description}
      aria-label={cost.description}
      data-cost-tier={cost.tier}
    >
      {PIPS.map((pip) => (
        <span
          key={pip}
          className={pip <= cost.tier ? "model-cost-pip model-cost-pip-filled" : "model-cost-pip"}
          aria-hidden="true"
        />
      ))}
    </span>
  );
}

export default ModelCostIndicator;
