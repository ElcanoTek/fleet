import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { renderHook } from "@testing-library/react";
import { useVisibleInterval } from "./useVisibleInterval";

let visibility: DocumentVisibilityState = "visible";

function setVisibility(next: DocumentVisibilityState, dispatch = true) {
  visibility = next;
  if (dispatch) document.dispatchEvent(new Event("visibilitychange"));
}

beforeEach(() => {
  vi.useFakeTimers();
  visibility = "visible";
  Object.defineProperty(document, "visibilityState", { configurable: true, get: () => visibility });
});

afterEach(() => {
  vi.useRealTimers();
  delete (document as { visibilityState?: unknown }).visibilityState;
});

describe("useVisibleInterval", () => {
  it("ticks on the interval while visible, never on mount", () => {
    const fn = vi.fn();
    renderHook(() => useVisibleInterval(fn, 1000));
    expect(fn).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1000);
    expect(fn).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(2000);
    expect(fn).toHaveBeenCalledTimes(3);
  });

  it("skips ticks while hidden and fires once on becoming visible", () => {
    const fn = vi.fn();
    renderHook(() => useVisibleInterval(fn, 1000));
    setVisibility("hidden");
    expect(fn).not.toHaveBeenCalled();
    vi.advanceTimersByTime(5000);
    expect(fn).not.toHaveBeenCalled();
    setVisibility("visible");
    expect(fn).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(1000);
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it("does nothing while disabled, and stops on unmount", () => {
    const fn = vi.fn();
    const { rerender, unmount } = renderHook(({ on }) => useVisibleInterval(fn, 1000, on), {
      initialProps: { on: false },
    });
    vi.advanceTimersByTime(3000);
    setVisibility("hidden");
    setVisibility("visible");
    expect(fn).not.toHaveBeenCalled();
    rerender({ on: true });
    vi.advanceTimersByTime(1000);
    expect(fn).toHaveBeenCalledTimes(1);
    unmount();
    vi.advanceTimersByTime(5000);
    setVisibility("visible");
    expect(fn).toHaveBeenCalledTimes(1);
  });

  it("calls the latest fn without re-arming the timer", () => {
    const first = vi.fn();
    const second = vi.fn();
    const { rerender } = renderHook(({ fn }) => useVisibleInterval(fn, 1000), {
      initialProps: { fn: first },
    });
    vi.advanceTimersByTime(600);
    rerender({ fn: second });
    vi.advanceTimersByTime(400);
    expect(first).not.toHaveBeenCalled();
    expect(second).toHaveBeenCalledTimes(1);
  });
});
