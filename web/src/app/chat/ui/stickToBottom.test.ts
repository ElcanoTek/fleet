import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, renderHook } from "@testing-library/react";
import {
  FOLLOW_BOTTOM_EPSILON_PX,
  IDLE_FOLLOW_WINDOW_PX,
  SMOOTH_FOLLOW_SETTLE_MS,
  STREAMING_FOLLOW_WINDOW_PX,
  decideFollowBottom,
  useStickToBottom,
  type FollowBottomInput,
} from "./stickToBottom";

const decide = (over: Partial<FollowBottomInput> = {}) =>
  decideFollowBottom({
    metrics: { scrollTop: 0, scrollHeight: 1000, clientHeight: 1000 },
    isStreaming: false,
    previousScrollHeight: null,
    smoothFollowInFlight: false,
    prefersReducedMotion: false,
    ...over,
  });

/** Metrics for "pinned at the bottom of a `height`-tall document". */
const atBottom = (height: number, clientHeight = 600) => ({
  scrollTop: height - clientHeight,
  scrollHeight: height,
  clientHeight,
});

afterEach(cleanup);

describe("decideFollowBottom", () => {
  it("follows on the first run, with no baseline to compare against", () => {
    expect(decide({ metrics: atBottom(2000) })).toEqual({
      follow: true,
      behavior: "smooth",
    });
  });

  it("follows when the content genuinely grew", () => {
    expect(
      decide({ metrics: atBottom(2000), previousScrollHeight: 1900 }),
    ).toMatchObject({ follow: true });
  });

  // The loop breaker: pinned at the bottom, nothing grew past measurement
  // noise. Re-scrolling here only re-runs measurement and invites the next
  // pixel of jitter — which is the shake QA saw.
  it("does not re-fire on a sub-pixel height delta while pinned", () => {
    expect(
      decide({
        metrics: { scrollTop: 1400.4, scrollHeight: 2000.4, clientHeight: 600 },
        previousScrollHeight: 2000,
      }),
    ).toEqual({ follow: false, reason: "pinned-and-unchanged" });
  });

  it("does not re-fire on a delta below the epsilon, in either direction", () => {
    const shrunk = decide({
      metrics: atBottom(2000),
      previousScrollHeight: 2000 + (FOLLOW_BOTTOM_EPSILON_PX - 1),
    });
    const grown = decide({
      metrics: atBottom(2000),
      previousScrollHeight: 2000 - (FOLLOW_BOTTOM_EPSILON_PX - 1),
    });
    expect(shrunk).toEqual({ follow: false, reason: "pinned-and-unchanged" });
    expect(grown).toEqual({ follow: false, reason: "pinned-and-unchanged" });
  });

  it("follows again once the delta clears the epsilon", () => {
    expect(
      decide({
        metrics: atBottom(2000),
        previousScrollHeight: 2000 - FOLLOW_BOTTOM_EPSILON_PX,
      }),
    ).toMatchObject({ follow: true });
  });

  it("still follows when unchanged but the reader is off the bottom", () => {
    // Nothing grew, but we are not pinned — e.g. a row above collapsed. The
    // guard is about not answering jitter, not about refusing to catch up.
    expect(
      decide({
        metrics: { scrollTop: 1300, scrollHeight: 2000, clientHeight: 600 },
        previousScrollHeight: 2000,
      }),
    ).toMatchObject({ follow: true });
  });

  it("leaves a reader who scrolled away alone", () => {
    const idle = decide({
      metrics: {
        scrollTop: 0,
        scrollHeight: 2000,
        clientHeight: 600,
      },
    });
    expect(idle).toEqual({ follow: false, reason: "user-scrolled-away" });
  });

  it("uses a wider follow window mid-stream than at rest", () => {
    const distance = (IDLE_FOLLOW_WINDOW_PX + STREAMING_FOLLOW_WINDOW_PX) / 2;
    const metrics = { scrollTop: 0, scrollHeight: 600 + distance, clientHeight: 600 };
    expect(decide({ metrics, isStreaming: false })).toMatchObject({ follow: false });
    expect(decide({ metrics, isStreaming: true })).toMatchObject({ follow: true });
  });

  it("skips a second smooth follow while the first is still animating", () => {
    expect(
      decide({ metrics: atBottom(2000), smoothFollowInFlight: true }),
    ).toEqual({ follow: false, reason: "smooth-follow-in-flight" });
  });

  it("does not let the in-flight guard block an instant follow", () => {
    // Instant scrolls are applied synchronously — there is nothing in flight
    // to collide with, and blocking them would stall a live stream.
    expect(
      decide({ metrics: atBottom(2000), isStreaming: true, smoothFollowInFlight: true }),
    ).toEqual({ follow: true, behavior: "auto" });
  });

  it("scrolls instantly while streaming and under reduced motion", () => {
    expect(decide({ metrics: atBottom(2000), isStreaming: true })).toMatchObject({
      behavior: "auto",
    });
    expect(
      decide({ metrics: atBottom(2000), prefersReducedMotion: true }),
    ).toMatchObject({ behavior: "auto" });
  });

  it("follows when there is no scroll container to measure yet", () => {
    expect(decide({ metrics: null })).toEqual({ follow: true, behavior: "smooth" });
  });
});

describe("useStickToBottom", () => {
  function harness() {
    const scrollIntoView = vi.fn();
    const sentinel = { scrollIntoView } as unknown as HTMLDivElement;
    // A plain mutable stand-in: the hook only reads the three metrics, and a
    // real HTMLElement's scroll properties are read-only, so a test cannot
    // simulate the height jitter this guard exists for.
    const scroller = { scrollTop: 1400, scrollHeight: 2000, clientHeight: 600 };
    return {
      scrollIntoView,
      scroller,
      refs: {
        conversationRef: { current: scroller as unknown as HTMLElement },
        streamEndRef: { current: sentinel },
      },
    };
  }

  it("follows once, then ignores a jittering height", () => {
    const { scrollIntoView, scroller, refs } = harness();
    const { rerender } = renderHook(
      ({ messages }: { messages: readonly unknown[] }) =>
        useStickToBottom({ ...refs, messages, isStreaming: false }),
      { initialProps: { messages: [1] as readonly unknown[] } },
    );
    expect(scrollIntoView).toHaveBeenCalledTimes(1);
    expect(scrollIntoView).toHaveBeenCalledWith({ block: "end", behavior: "smooth" });

    // A re-measure moves the total size by a pixel. Without the guards this is
    // the second follow that starts the oscillation.
    scroller.scrollHeight = 2001;
    scroller.scrollTop = 1401;
    rerender({ messages: [1, 2] });
    expect(scrollIntoView).toHaveBeenCalledTimes(1);
  });

  it("re-follows after the smooth scroll settles and real content arrives", () => {
    vi.useFakeTimers();
    try {
      const { scrollIntoView, scroller, refs } = harness();
      const { rerender } = renderHook(
        ({ messages }: { messages: readonly unknown[] }) =>
          useStickToBottom({ ...refs, messages, isStreaming: false }),
        { initialProps: { messages: [1] as readonly unknown[] } },
      );
      expect(scrollIntoView).toHaveBeenCalledTimes(1);

      // Real growth, but the first smooth scroll is still animating.
      scroller.scrollHeight = 2400;
      scroller.scrollTop = 1800;
      rerender({ messages: [1, 2] });
      expect(scrollIntoView).toHaveBeenCalledTimes(1);

      vi.advanceTimersByTime(SMOOTH_FOLLOW_SETTLE_MS);
      scroller.scrollHeight = 2800;
      scroller.scrollTop = 2200;
      rerender({ messages: [1, 2, 3] });
      expect(scrollIntoView).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("does nothing without a sentinel", () => {
    const scrollIntoView = vi.fn();
    renderHook(() =>
      useStickToBottom({
        conversationRef: { current: null },
        streamEndRef: { current: null },
        messages: [1],
        isStreaming: false,
      }),
    );
    expect(scrollIntoView).not.toHaveBeenCalled();
  });
});
