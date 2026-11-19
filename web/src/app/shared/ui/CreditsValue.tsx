import { t as tr } from "@/app/lib/i18n";
import { formatCreditAmount } from "@/app/shared/lib/credits";

// CreditsValue — a credit amount for a headline stat tile: the number at the
// tile's own size with "credits" as a small unit suffix on the same line, so a
// large figure never wraps the unit onto a second row. Inline text elsewhere
// uses formatCredits (shared/lib/credits.ts); the two read identically
// ("212.45 credits") to assistive technology and in textContent.
export function CreditsValue({ value, digits }: { value: number | null | undefined; digits?: number }) {
  return (
    <span className="credits-value">
      {formatCreditAmount(value, digits)} <span className="credits-unit">{tr("credits")}</span>
    </span>
  );
}

export default CreditsValue;
