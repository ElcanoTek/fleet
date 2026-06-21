// Context-usage display helper.
//
// Drives the "180k / 1M (18%)" indicator that appears next to total
// cost when the user has Show details on. Pure so the same predicate
// tests cover both the chip rendering and any future warning surface.
//
// "Used" tokens come from the most recent turn_summary's prompt_tokens
// — that's exactly what the agent sent on the last turn, which is the
// best proxy for "what the next turn's prompt will look like before
// the user types another character." Capacity is the model's
// context_length from the OpenRouter catalog when known.

export type ContextUsageInputs = {
  // Most recent turn's prompt_tokens. 0 / undefined / negative means
  // "no live data yet" — the chip should not render.
  promptTokens: number | null | undefined;
  // Currently-selected model's total context window (prompt +
  // completion). Optional; when undefined the indicator falls back to
  // showing just the numerator.
  contextLength: number | null | undefined;
};

export type ContextSeverity = "ok" | "warn" | "danger";

export type ContextUsage = {
  used: number;
  capacity: number | undefined;
  fraction: number | undefined; // [0..1+] — may exceed 1 if usage > capacity
  severity: ContextSeverity;
};

// Anchor points for the color shift. 70% / 90% chosen empirically:
// Anthropic + Google models both start showing degraded recall around
// the 70% mark on long contexts, and 90% is the danger zone where the
// next attached file would push us over.
export const CONTEXT_WARN_FRACTION = 0.7;
export const CONTEXT_DANGER_FRACTION = 0.9;

export function severityFor(fraction: number | undefined): ContextSeverity {
  if (fraction === undefined) return "ok";
  if (fraction >= CONTEXT_DANGER_FRACTION) return "danger";
  if (fraction >= CONTEXT_WARN_FRACTION) return "warn";
  return "ok";
}

export function computeContextUsage(input: ContextUsageInputs): ContextUsage | null {
  const used = typeof input.promptTokens === "number" ? input.promptTokens : 0;
  if (!Number.isFinite(used) || used <= 0) return null;
  const capRaw = input.contextLength;
  const capacity =
    typeof capRaw === "number" && Number.isFinite(capRaw) && capRaw > 0
      ? capRaw
      : undefined;
  const fraction = capacity !== undefined ? used / capacity : undefined;
  return {
    used,
    capacity,
    fraction,
    severity: severityFor(fraction),
  };
}

// formatContextUsage produces the display string for the chip — kept
// here so the chip renders deterministically without re-implementing
// the same `k`/`M` rounding rules that `formatTokens` uses elsewhere.
function formatTokens(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`;
  return `${(n / 1_000_000).toFixed(2)}M`;
}

export function formatContextUsage(usage: ContextUsage): string {
  if (usage.capacity === undefined || usage.fraction === undefined) {
    return `${formatTokens(usage.used)} ctx`;
  }
  // Defensive clamp at 100%. fraction > 1 is an impossible state for a
  // turn that succeeded — if we see one it means either (a) the
  // OpenRouter catalog's `context_length` for this model is stale
  // (e.g. preview model bumped from 200k→1M after our 24h cache
  // started) or (b) we're reading a legacy summary that still carries
  // the summed-across-steps promptTokens. Either way, displaying
  // "200%" alarms the user about a non-real overflow. Showing
  // "100%+" makes it clear the chat is near capacity without
  // implying an impossible-and-also-not-yet-rejected state.
  const pct =
    usage.fraction > 1
      ? "100%+"
      : `${Math.max(0, Math.round(usage.fraction * 100))}%`;
  return `${formatTokens(usage.used)} / ${formatTokens(usage.capacity)} ctx (${pct})`;
}

// shouldShowDegradationCaption returns true once we cross into the
// "warn" band. Drives the one-line caption that puts some onus on the
// user: "Long chats may produce shallower analysis as context fills."
export function shouldShowDegradationCaption(usage: ContextUsage | null): boolean {
  if (!usage) return false;
  return usage.severity !== "ok";
}
