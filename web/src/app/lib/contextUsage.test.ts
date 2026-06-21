import { describe, expect, it } from "vitest";
import {
  CONTEXT_DANGER_FRACTION,
  CONTEXT_WARN_FRACTION,
  computeContextUsage,
  formatContextUsage,
  severityFor,
  shouldShowDegradationCaption,
} from "./contextUsage";

describe("computeContextUsage", () => {
  it("returns null when there is no prompt_tokens yet", () => {
    expect(computeContextUsage({ promptTokens: 0, contextLength: 1_000_000 })).toBeNull();
    expect(computeContextUsage({ promptTokens: null, contextLength: 1_000_000 })).toBeNull();
    expect(computeContextUsage({ promptTokens: undefined, contextLength: 1_000_000 })).toBeNull();
  });

  it("returns just the numerator when context length is unknown", () => {
    const usage = computeContextUsage({ promptTokens: 12_345, contextLength: undefined });
    expect(usage).toEqual({
      used: 12_345,
      capacity: undefined,
      fraction: undefined,
      severity: "ok",
    });
  });

  it("computes the fraction when capacity is known", () => {
    const usage = computeContextUsage({ promptTokens: 250_000, contextLength: 1_000_000 });
    expect(usage?.fraction).toBeCloseTo(0.25);
    expect(usage?.severity).toBe("ok");
  });

  it("flags the warn band at and above 70%", () => {
    const at = computeContextUsage({
      promptTokens: Math.round(CONTEXT_WARN_FRACTION * 1_000_000),
      contextLength: 1_000_000,
    });
    expect(at?.severity).toBe("warn");
  });

  it("flags the danger band at and above 90%", () => {
    const at = computeContextUsage({
      promptTokens: Math.round(CONTEXT_DANGER_FRACTION * 1_000_000),
      contextLength: 1_000_000,
    });
    expect(at?.severity).toBe("danger");
  });

  it("rejects nonsense capacity values", () => {
    const usage = computeContextUsage({ promptTokens: 12_345, contextLength: 0 });
    expect(usage?.capacity).toBeUndefined();
    expect(usage?.fraction).toBeUndefined();
  });
});

describe("severityFor", () => {
  it("treats undefined fraction as ok (capacity unknown)", () => {
    expect(severityFor(undefined)).toBe("ok");
  });
  it("orders the bands correctly", () => {
    expect(severityFor(0)).toBe("ok");
    expect(severityFor(0.5)).toBe("ok");
    expect(severityFor(0.7)).toBe("warn");
    expect(severityFor(0.89)).toBe("warn");
    expect(severityFor(0.9)).toBe("danger");
    expect(severityFor(1.5)).toBe("danger");
  });
});

describe("formatContextUsage", () => {
  it("prints capacity when known", () => {
    const usage = computeContextUsage({ promptTokens: 250_000, contextLength: 1_000_000 })!;
    expect(formatContextUsage(usage)).toBe("250k / 1.00M ctx (25%)");
  });

  it("omits capacity gracefully when unknown", () => {
    const usage = computeContextUsage({ promptTokens: 18_500, contextLength: undefined })!;
    // 18.5k rounds to 19k under our zero-decimal display rule for the
    // >=10k band — overstating slightly is fine here, understating is
    // not (we don't want users to think they have more headroom than
    // they do).
    expect(formatContextUsage(usage)).toBe("19k ctx");
  });

  it("keeps one decimal in the 1k–9.9k band", () => {
    const usage = computeContextUsage({ promptTokens: 4_250, contextLength: undefined })!;
    expect(formatContextUsage(usage)).toBe("4.3k ctx");
  });

  it("clamps over-capacity fractions to 100%+ instead of an impossible percent", () => {
    // Production incident (Jeanne, conv 3460d911): a multi-step
    // agentic turn's accumulated promptTokens (813k) divided by a
    // stale-or-misreported context_length produced "200%" in the
    // indicator. fraction > 1 means our model knowledge is stale OR
    // we're reading a legacy summary that still summed promptTokens
    // across steps — either way, displaying "200%" alarms the user
    // about a non-real overflow. "100%+" is the honest signal.
    const usage = computeContextUsage({ promptTokens: 50_000_000, contextLength: 1_000_000 })!;
    expect(formatContextUsage(usage)).toBe("50.00M / 1.00M ctx (100%+)");

    // Just barely over should also clamp.
    const barelyOver = computeContextUsage({ promptTokens: 1_000_001, contextLength: 1_000_000 })!;
    expect(formatContextUsage(barelyOver)).toBe("1.00M / 1.00M ctx (100%+)");

    // The "Jeanne case" — 813k summed promptTokens against a stale
    // 400k-context model. Pre-fix this rendered "203%".
    const jeanne = computeContextUsage({ promptTokens: 813_277, contextLength: 400_000 })!;
    expect(formatContextUsage(jeanne)).toBe("813k / 400k ctx (100%+)");
  });

  it("still renders sub-100% percents normally", () => {
    const usage = computeContextUsage({ promptTokens: 250_000, contextLength: 1_000_000 })!;
    expect(formatContextUsage(usage)).toBe("250k / 1.00M ctx (25%)");
    const warn = computeContextUsage({ promptTokens: 800_000, contextLength: 1_000_000 })!;
    expect(formatContextUsage(warn)).toBe("800k / 1.00M ctx (80%)");
  });
});

describe("shouldShowDegradationCaption", () => {
  it("is silent when usage is null", () => {
    expect(shouldShowDegradationCaption(null)).toBe(false);
  });

  it("is silent below the warn band", () => {
    const usage = computeContextUsage({ promptTokens: 100_000, contextLength: 1_000_000 })!;
    expect(shouldShowDegradationCaption(usage)).toBe(false);
  });

  it("fires from the warn band onwards", () => {
    const warn = computeContextUsage({ promptTokens: 720_000, contextLength: 1_000_000 })!;
    const danger = computeContextUsage({ promptTokens: 950_000, contextLength: 1_000_000 })!;
    expect(shouldShowDegradationCaption(warn)).toBe(true);
    expect(shouldShowDegradationCaption(danger)).toBe(true);
  });
});
