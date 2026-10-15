import { describe, expect, it, vi } from "vitest";
import { renderHook } from "@testing-library/react";
import { advancedRetryAllowed, useQueuedSwitchRetry } from "./useQueuedSwitchRetry";

describe("useQueuedSwitchRetry", () => {
  it("retries at once when no turn is streaming", () => {
    const retry = vi.fn();
    const { result } = renderHook(() => useQueuedSwitchRetry(false, retry));
    void result.current("adv/model");
    expect(retry).toHaveBeenCalledWith({ model: "adv/model" });
  });

  // A card staged after the reply can be clicked before the stream closes;
  // the retry waits for it instead of being dropped.
  it("holds a click made while streaming and sends it once streaming ends", () => {
    const retry = vi.fn();
    const { result, rerender } = renderHook(({ streaming }) => useQueuedSwitchRetry(streaming, retry), {
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

describe("advancedRetryAllowed", () => {
  it("is offered outside lockdown, and in lockdown only when the list names the model", () => {
    expect(advancedRetryAllowed(false, [], "adv/model")).toBe(true);
    expect(advancedRetryAllowed(true, ["fast/model", "adv/model"], "adv/model")).toBe(true);
    expect(advancedRetryAllowed(true, ["fast/model"], "adv/model")).toBe(false);
    expect(advancedRetryAllowed(true, [], "adv/model")).toBe(false);
  });
});
