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

  // A height-only change at rest: rows are re-measured as they scroll into
  // view, so the idle window would pull a reader back down by their own
  // scrolling. Only a reader who was pinned before the growth is followed.
  it("leaves a reader who scrolled up a little alone when only the height changed", () => {
    expect(
      decide({
        // 80px above the end; a row re-measured 6px taller.
        metrics: { scrollTop: 1326, scrollHeight: 2006, clientHeight: 600 },
        previousScrollHeight: 2000,
        heightOnly: true,
      }),
    ).toEqual({ follow: false, reason: "user-scrolled-away" });
  });

  it("follows a height-only growth when the reader was pinned before it", () => {
    expect(
      decide({
        // Was at the end of 2000; the reply's action row added 44px.
        metrics: { scrollTop: 1400, scrollHeight: 2044, clientHeight: 600 },
        previousScrollHeight: 2000,
        heightOnly: true,
      }),
    ).toEqual({ follow: true, behavior: "smooth" });
  });

  it("does not count a shrink as having been pinned", () => {
    expect(
      decide({
        metrics: { scrollTop: 1300, scrollHeight: 1960, clientHeight: 600 },
        previousScrollHeight: 2000,
        heightOnly: true,
      }),
    ).toEqual({ follow: false, reason: "user-scrolled-away" });
  });

  it("keeps the wide window for a height-only change mid-stream", () => {
    expect(
      decide({
        metrics: { scrollTop: 1300, scrollHeight: 2000, clientHeight: 600 },
        previousScrollHeight: 2000,
        isStreaming: true,
        heightOnly: true,
      }),
    ).toEqual({ follow: true, behavior: "auto" });
  });

  it("follows when there is no scroll container to measure yet", () => {
    expect(decide({ metrics: null })).toEqual({ follow: true, behavior: "smooth" });
  });

  // With the scroll record: "pinned" is known, not inferred from distances.
  it("carries a pinned reader past the follow window when a new message moves the end", () => {
    // Their own 700px message: far beyond either window, and they never moved.
    const metrics = { scrollTop: 1400, scrollHeight: 2700, clientHeight: 600 };
    expect(decide({ metrics, previousScrollHeight: 2000, wasPinned: true })).toMatchObject({ follow: true });
    expect(
      decide({ metrics, previousScrollHeight: 2000, wasPinned: true, isStreaming: true, heightOnly: true }),
    ).toEqual({ follow: true, behavior: "auto" });
  });

  it("does not carry a reader who scrolled up past the window", () => {
    expect(
      decide({
        metrics: { scrollTop: 1400, scrollHeight: 2700, clientHeight: 600 },
        previousScrollHeight: 2000,
        wasPinned: false,
      }),
    ).toEqual({ follow: false, reason: "user-scrolled-away" });
  });

  it("follows a resting re-measure for a pinned reader even when the growth cannot be seen", () => {
    // A row shrank and regrew between two looks: the baseline is higher than
    // the height now, so the growth measures as nothing.
    expect(
      decide({
        metrics: { scrollTop: 1353, scrollHeight: 2000, clientHeight: 600 },
        previousScrollHeight: 2033,
        heightOnly: true,
        wasPinned: true,
      }),
    ).toMatchObject({ follow: true });
  });

  it("does not follow a resting re-measure for a reader who scrolled up, whatever grew", () => {
    // The growth equals the distance, which the estimate would read as pinned.
    expect(
      decide({
        metrics: { scrollTop: 1400, scrollHeight: 2040, clientHeight: 600 },
        previousScrollHeight: 2000,
        heightOnly: true,
        wasPinned: false,
      }),
    ).toEqual({ follow: false, reason: "user-scrolled-away" });
  });

  it("keeps the window for a resting re-measure even when pinned", () => {
    // Opening a tall panel in the last reply: do not scroll its toggle away.
    expect(
      decide({
        metrics: { scrollTop: 1400, scrollHeight: 2600, clientHeight: 600 },
        previousScrollHeight: 2000,
        heightOnly: true,
        wasPinned: true,
      }),
    ).toEqual({ follow: false, reason: "user-scrolled-away" });
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

  // A virtual row is measured after the render that changed it: when a reply
  // finishes, its action row mounts and the transcript grows with the message
  // list untouched. That growth used to trigger nothing, leaving the newest
  // reply's actions under the composer.
  it("follows when only the measured content height changes", () => {
    vi.useFakeTimers();
    try {
      const { scrollIntoView, scroller, refs } = harness();
      const messages = [1] as readonly unknown[];
      const { rerender } = renderHook(
        ({ contentHeight }: { contentHeight: number }) =>
          useStickToBottom({ ...refs, messages, isStreaming: false, contentHeight }),
        { initialProps: { contentHeight: 2000 } },
      );
      expect(scrollIntoView).toHaveBeenCalledTimes(1);
      vi.advanceTimersByTime(SMOOTH_FOLLOW_SETTLE_MS);

      // The finished reply's action row is measured: +44px, reader was pinned.
      scroller.scrollHeight = 2044;
      rerender({ contentHeight: 2044 });
      expect(scrollIntoView).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("re-checks once the smooth follow settles when growth landed mid-animation", () => {
    vi.useFakeTimers();
    try {
      const { scrollIntoView, scroller, refs } = harness();
      const messages = [1] as readonly unknown[];
      const { rerender } = renderHook(
        ({ contentHeight }: { contentHeight: number }) =>
          useStickToBottom({ ...refs, messages, isStreaming: false, contentHeight }),
        { initialProps: { contentHeight: 2000 } },
      );
      expect(scrollIntoView).toHaveBeenCalledTimes(1);

      // Growth while the first smooth follow is still animating: skipped now…
      scroller.scrollHeight = 2044;
      rerender({ contentHeight: 2044 });
      expect(scrollIntoView).toHaveBeenCalledTimes(1);

      // …and picked up when it settles, with no further content change. The
      // animation ended where it was aimed, 44px short of the new end.
      vi.advanceTimersByTime(SMOOTH_FOLLOW_SETTLE_MS);
      expect(scrollIntoView).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  // The regression this guards: a reader scrolls up 80px at rest, rows near
  // the viewport are re-measured, and the transcript's height moves a little.
  it("does not pull a reader back when a re-measure follows their own scroll", () => {
    vi.useFakeTimers();
    try {
      const { scrollIntoView, scroller, refs } = harness();
      const messages = [1] as readonly unknown[];
      const { rerender } = renderHook(
        ({ contentHeight }: { contentHeight: number }) =>
          useStickToBottom({ ...refs, messages, isStreaming: false, contentHeight }),
        { initialProps: { contentHeight: 2000 } },
      );
      expect(scrollIntoView).toHaveBeenCalledTimes(1);
      vi.advanceTimersByTime(SMOOTH_FOLLOW_SETTLE_MS);

      scroller.scrollTop = 1320;
      scroller.scrollHeight = 2007;
      rerender({ contentHeight: 2007 });
      scroller.scrollHeight = 1998;
      rerender({ contentHeight: 1998 });
      expect(scrollIntoView).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("drops the settle re-check when the reader scrolled away mid-animation", () => {
    vi.useFakeTimers();
    try {
      const { scrollIntoView, scroller, refs } = harness();
      const messages = [1] as readonly unknown[];
      const { rerender } = renderHook(
        ({ contentHeight }: { contentHeight: number }) =>
          useStickToBottom({ ...refs, messages, isStreaming: false, contentHeight }),
        { initialProps: { contentHeight: 2000 } },
      );
      scroller.scrollHeight = 2044;
      rerender({ contentHeight: 2044 });
      // The reader interrupts the animation and ends 100px above the old end.
      scroller.scrollTop = 1300;
      vi.advanceTimersByTime(SMOOTH_FOLLOW_SETTLE_MS);
      expect(scrollIntoView).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  // A scroller that can be listened to and driven, as the real one can.
  function liveHarness() {
    const listeners: Array<() => void> = [];
    const scroller = {
      scrollTop: 1400,
      scrollHeight: 2000,
      clientHeight: 600,
      addEventListener: (_: string, fn: () => void) => void listeners.push(fn),
      removeEventListener: () => {},
      scrollTo: vi.fn(),
    };
    return {
      scroller,
      userScrollsTo(top: number) {
        scroller.scrollTop = top;
        for (const fn of listeners) fn();
      },
      refs: {
        conversationRef: { current: scroller as unknown as HTMLElement },
        streamEndRef: { current: { scrollIntoView: vi.fn() } as unknown as HTMLDivElement },
      },
    };
  }

  it("follows a reader at the end when their own long message moves it beyond the window", () => {
    vi.useFakeTimers();
    try {
      const { scroller, refs } = liveHarness();
      const { rerender } = renderHook(
        ({ messages }: { messages: readonly unknown[] }) =>
          useStickToBottom({ ...refs, messages, isStreaming: false }),
        { initialProps: { messages: [1] as readonly unknown[] } },
      );
      expect(scroller.scrollTo).toHaveBeenCalledTimes(1);
      vi.advanceTimersByTime(SMOOTH_FOLLOW_SETTLE_MS);

      scroller.scrollHeight = 2700;
      rerender({ messages: [1, 2, 3] });
      expect(scroller.scrollTo).toHaveBeenCalledTimes(2);
      expect(scroller.scrollTo).toHaveBeenLastCalledWith({ top: 2700, behavior: "smooth" });
    } finally {
      vi.useRealTimers();
    }
  });

  it("leaves a reader who scrolled up where they are when a new message lands far below", () => {
    vi.useFakeTimers();
    try {
      const { scroller, userScrollsTo, refs } = liveHarness();
      const { rerender } = renderHook(
        ({ messages }: { messages: readonly unknown[] }) =>
          useStickToBottom({ ...refs, messages, isStreaming: false }),
        { initialProps: { messages: [1] as readonly unknown[] } },
      );
      vi.advanceTimersByTime(SMOOTH_FOLLOW_SETTLE_MS);
      userScrollsTo(1400);
      userScrollsTo(1100);

      scroller.scrollHeight = 2700;
      rerender({ messages: [1, 2, 3] });
      expect(scroller.scrollTo).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("follows a pinned reader when a row shrank and regrew between two looks", () => {
    vi.useFakeTimers();
    try {
      const { scroller, userScrollsTo, refs } = liveHarness();
      const messages = [1] as readonly unknown[];
      const { rerender } = renderHook(
        ({ contentHeight }: { contentHeight: number }) =>
          useStickToBottom({ ...refs, messages, isStreaming: false, contentHeight }),
        { initialProps: { contentHeight: 2000 } },
      );
      vi.advanceTimersByTime(SMOOTH_FOLLOW_SETTLE_MS);

      // The row shrinks; the browser clamps the position, still at the end.
      scroller.scrollHeight = 1953;
      userScrollsTo(1353);
      // It regrows before the next look: 47px now sit below the fold, and the
      // height is the one the last look already saw.
      scroller.scrollHeight = 2000;
      rerender({ contentHeight: 2001 });
      expect(scroller.scrollTo).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("returns a reader to pinned once they scroll back to the end", () => {
    vi.useFakeTimers();
    try {
      const { scroller, userScrollsTo, refs } = liveHarness();
      const { rerender } = renderHook(
        ({ messages }: { messages: readonly unknown[] }) =>
          useStickToBottom({ ...refs, messages, isStreaming: false }),
        { initialProps: { messages: [1] as readonly unknown[] } },
      );
      vi.advanceTimersByTime(SMOOTH_FOLLOW_SETTLE_MS);
      userScrollsTo(1400);
      userScrollsTo(1100);
      userScrollsTo(1400);

      scroller.scrollHeight = 2700;
      rerender({ messages: [1, 2, 3] });
      expect(scroller.scrollTo).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("scrolls the container to its true end when it can be driven", () => {
    const { scrollIntoView, scroller, refs } = harness();
    const scrollTo = vi.fn();
    (scroller as unknown as { scrollTo: typeof scrollTo }).scrollTo = scrollTo;
    renderHook(() =>
      useStickToBottom({ ...refs, messages: [1], isStreaming: false }),
    );
    expect(scrollTo).toHaveBeenCalledWith({ top: 2000, behavior: "smooth" });
    expect(scrollIntoView).not.toHaveBeenCalled();
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
