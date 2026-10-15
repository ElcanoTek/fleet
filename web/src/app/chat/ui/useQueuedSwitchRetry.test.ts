import { describe, expect, it, vi } from "vitest";
import { renderHook } from "@testing-library/react";
import { advancedRetryAllowed, hasPendingSwitchSuggestion, useQueuedSwitchRetry } from "./useQueuedSwitchRetry";

describe("useQueuedSwitchRetry", () => {
  it("retries at once when no turn is streaming", () => {
    const retry = vi.fn();
    const { result } = renderHook(() => useQueuedSwitchRetry(false, "conv-a", retry));
    void result.current("adv/model");
    expect(retry).toHaveBeenCalledWith({ model: "adv/model" });
  });

  // A card staged after the reply can be clicked before the stream closes;
  // the retry waits for it instead of being dropped.
  it("holds a click made while streaming and sends it once streaming ends", () => {
    const retry = vi.fn();
    const { result, rerender } = renderHook(({ streaming }) => useQueuedSwitchRetry(streaming, "conv-a", retry), {
      initialProps: { streaming: true },
    });
    void result.current("adv/model");
    expect(retry).not.toHaveBeenCalled();
    rerender({ streaming: false });
    expect(retry).toHaveBeenCalledTimes(1);
    expect(retry).toHaveBeenCalledWith({ model: "adv/model" });
    rerender({ streaming: true });
    rerender({ streaming: false });
    expect(retry).toHaveBeenCalledTimes(1);
  });
});

// Retry acts on the active conversation, so a click held in one conversation
// must not run in another the user moved to before the stream closed.
describe("useQueuedSwitchRetry across conversations", () => {
  it("drops a held retry when the active conversation changes", () => {
    const retry = vi.fn();
    const { result, rerender } = renderHook(
      ({ streaming, conv }) => useQueuedSwitchRetry(streaming, conv, retry),
      { initialProps: { streaming: true, conv: "conv-a" } },
    );
    void result.current("adv/model");
    rerender({ streaming: false, conv: "conv-b" });
    rerender({ streaming: false, conv: "conv-a" });
    expect(retry).not.toHaveBeenCalled();
  });
});

describe("advancedRetryAllowed", () => {
  it("is offered outside lockdown, and in lockdown only when the list names the model", () => {
    expect(advancedRetryAllowed(false, [], "adv/model")).toBe(true);
    expect(advancedRetryAllowed(true, ["fast/model", "adv/model"], "adv/model")).toBe(true);
    expect(advancedRetryAllowed(true, ["fast/model"], "adv/model")).toBe(false);
    expect(advancedRetryAllowed(true, [], "adv/model")).toBe(false);
  });
});

describe("hasPendingSwitchSuggestion", () => {
  it("is true only while a model-switch card on the reply is still pending", () => {
    expect(hasPendingSwitchSuggestion(undefined)).toBe(false);
    expect(hasPendingSwitchSuggestion([{ tool: "suggest_advanced_model", status: "pending" }])).toBe(true);
    expect(hasPendingSwitchSuggestion([{ tool: "suggest_advanced_model", status: "rejected" }])).toBe(false);
    expect(hasPendingSwitchSuggestion([{ tool: "send_email", status: "pending" }])).toBe(false);
  });
});
