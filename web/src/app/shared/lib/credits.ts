import { formatNumber, t as tr } from "@/app/lib/i18n";

// Credits — how the UI shows money.
//
// fleet meters model spend in US dollars (the `*_usd` fields on the API, the
// FLEET_MAX_COST_USD ceiling, budgets). People using the product see that same
// number as "credits": one credit is one US dollar of metered spend, so this
// module only changes the label — no "$", no "USD" — never the amount. Every
// spend, cost, ceiling and budget figure the UI renders goes through here so
// the wording cannot drift between pages.

// formatCreditAmount renders the bare number ("1,234.50"), for a table cell
// or an input hint whose header already names the unit. By default it shows
// two decimals, widening to four for a non-zero amount under 0.01 so a
// 0.004-credit run does not read as the misleading "0.00". Pass `digits` for
// a fixed precision.
export function formatCreditAmount(value: number | null | undefined, digits?: number): string {
  const v = typeof value === "number" && Number.isFinite(value) ? value : 0;
  if (digits !== undefined) {
    return formatNumber(v, { minimumFractionDigits: digits, maximumFractionDigits: digits });
  }
  const fine = v > 0 && v < 0.01;
  return formatNumber(v, { minimumFractionDigits: 2, maximumFractionDigits: fine ? 4 : 2 });
}

// formatCredits renders the amount with its unit ("1,234.50 credits").
export function formatCredits(value: number | null | undefined, digits?: number): string {
  return tr("{0} credits", [formatCreditAmount(value, digits)]);
}
