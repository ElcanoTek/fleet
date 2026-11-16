import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, renderHook } from "@testing-library/react";
import {
  FOLLOW_BOTTOM_EPSILON_PX,
  IDLE_FOLLOW_WINDOW_PX,
  PIN_HOLD_MS,
  SMOOTH_FOLLOW_SETTLE_MS,
  STREAMING_FOLLOW_WINDOW_PX,
  decideFollowBottom,
  requestFollowBottom,
  requestHoldPosition,
  trackPin,
  useStickToBottom,
  type FollowBottomInput,
  type PinAnchor,
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

// A smooth follow of ours is under way: who is judged by distance, and who
// waits for it to land. One row per combination.
describe("decideFollowBottom while our own smooth follow is in flight", () => {
  const far = { scrollTop: 400, scrollHeight: 2000, clientHeight: 600 }; // 1000px from the end
  const near = { scrollTop: 1300, scrollHeight: 2000, clientHeight: 600 }; // 100px from the end
  it.each([
    // pinned, heightOnly, metrics, expected reason
    // Still pinned: wait for the landing, however far the animation has to go.
    [true, true, far, "smooth-follow-in-flight"],
    [true, false, far, "smooth-follow-in-flight"],
    [true, true, near, "smooth-follow-in-flight"],
    [true, false, near, "smooth-follow-in-flight"],
    // Scrolled up during the animation: they left, and distance says so.
    [false, true, far, "user-scrolled-away"],
    [false, false, far, "user-scrolled-away"],
    // Not known to be pinned, but near the end: the animation is still ours.
    [false, false, near, "smooth-follow-in-flight"],
  ] as const)("pinned=%s heightOnly=%s: %o -> %s", (wasPinned, heightOnly, metrics, reason) => {
    expect(
      decide({ metrics, previousScrollHeight: 1980, smoothFollowInFlight: true, heightOnly, wasPinned }),
    ).toEqual({ follow: false, reason });
  });

  // The re-check when that follow lands. The reader was being carried the
  // whole time, so they are carried the rest of the way, however much grew.
  it.each([
    // pinned, heightOnly, expected
    [true, true, { follow: true, behavior: "smooth" }],
    [true, false, { follow: true, behavior: "smooth" }],
    // They scrolled up while it was under way: left where they are.
    [false, true, { follow: false, reason: "user-scrolled-away" }],
    [false, false, { follow: false, reason: "user-scrolled-away" }],
  ] as const)("the landing re-check, pinned=%s heightOnly=%s, far from the end", (wasPinned, heightOnly, expected) => {
    expect(
      decide({ metrics: far, previousScrollHeight: 1700, heightOnly, wasPinned, afterOwnFollow: true }),
    ).toEqual(expected);
  });

  it("does not carry a reader at rest past the window just because a row grew (no follow of ours was under way)", () => {
    expect(decide({ metrics: far, previousScrollHeight: 1700, heightOnly: true, wasPinned: true })).toEqual({
      follow: false,
      reason: "user-scrolled-away",
    });
  });

  it("is judged by distance as before when streaming or under reduced motion (nothing is in flight)", () => {
    expect(
      decide({ metrics: far, previousScrollHeight: 1980, smoothFollowInFlight: true, heightOnly: true, wasPinned: true, prefersReducedMotion: true }),
    ).toEqual({ follow: false, reason: "user-scrolled-away" });
  });
});

// "Is the reader at the end", kept from scroll events. Each row is a sequence
// of events [top, distance from the end] starting pinned at the end of a
// 2000px transcript in a 600px box (top 1400), and whether they still count
// as at the end afterwards.
describe("trackPin", () => {
  const run = (events: ReadonlyArray<readonly [number, number]>, start: { pinned: boolean; anchor: PinAnchor | null }) =>
    events.reduce((state, [top, distance]) => trackPin(state.pinned, state.anchor, top, distance), start);
  const atEnd = { pinned: true, anchor: { top: 1400, distance: 0 } };
  const steps = (n: number, size: number) =>
    Array.from({ length: n }, (_, i) => [1400 - (i + 1) * size, (i + 1) * size] as const);

  it.each([
    // The reader scrolls up: in one jump, and slowly, in steps too small to
    // count on their own (a trackpad or a finger).
    ["one jump up", [[1100, 300]], false],
    ["80 steps of 1px", steps(80, 1), false],
    ["200 steps of 0.4px", steps(200, 0.4), false],
    ["10 steps of 1px", steps(10, 1), false],
    // Not the reader leaving.
    ["a couple of pixels from the end is still the end", steps(2, 1), true],
    ["new content below: the end moves away, they do not", [[1400, 300], [1400, 900]], true],
    ["our follow carries them down", [[1400, 600], [1700, 300], [2000, 0]], true],
    ["a row above shrinks: the position shifts, the distance does not", [[1400, 300], [1360, 300], [1300, 300]], true],
    ["a row above shrinks while they sit at the end", [[1360, 0]], true],
    // Content grew, then the reader scrolled up slowly from there.
    ["growth, then 5 steps of 1px up", [[1400, 300], [1399, 301], [1398, 302], [1397, 303], [1396, 304], [1395, 305]], false],
    // They scroll up and come back to the end.
    ["up and back down to the end", [[1100, 300], [1400, 0]], true],
  ] as const)("%s", (_name, events, pinned) => {
    expect(run(events, atEnd).pinned).toBe(pinned);
  });

  it("starts measuring from the first event after the app pins them (a send, Jump to latest)", () => {
    // Far up, pinned by the app, carried down by its smooth scroll: still pinned.
    expect(run([[600, 800], [1000, 400], [1400, 0]], { pinned: true, anchor: null }).pinned).toBe(true);
    // ... unless they scroll up against it.
    expect(run([[600, 800], [1000, 400], [990, 410], [960, 440]], { pinned: true, anchor: null }).pinned).toBe(false);
  });

  it("does not pin a reader who is scrolled up just because they scroll", () => {
    expect(run([[900, 500], [1000, 400], [950, 450]], { pinned: false, anchor: null }).pinned).toBe(false);
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

  // Where no scroll listener can be attached the hook falls back to its own
  // estimate of "was the reader at the end" (this harness has none, so the
  // four tests around here cover that fallback; the app's scroller is
  // listened to, and that path is tested with liveHarness below).
  // A reader scrolls up 80px at rest, rows near the viewport are re-measured,
  // and the transcript's height moves a little.
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

  // Sending a message, or pressing Jump to latest, is the reader asking to be
  // at the end: it holds wherever they had scrolled to.
  it("follows a reader who had scrolled up once they ask for the end", () => {
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
      userScrollsTo(500);

      requestFollowBottom();
      scroller.scrollHeight = 2900;
      rerender({ messages: [1, 2, 3] });
      expect(scroller.scrollTo).toHaveBeenCalledTimes(2);
      expect(scroller.scrollTo).toHaveBeenLastCalledWith({ top: 2900, behavior: "smooth" });
    } finally {
      vi.useRealTimers();
    }
  });

  // Jump to latest scrolls smoothly; a change that lands while it is still
  // on its way must not count as the reader having scrolled away again.
  it("keeps the pin through a change that outruns a requested jump", () => {
    vi.useFakeTimers();
    try {
      const { scroller, userScrollsTo, refs } = liveHarness();
      const one: readonly unknown[] = [1];
      const { rerender } = renderHook(
        ({ messages, contentHeight }: { messages: readonly unknown[]; contentHeight: number }) =>
          useStickToBottom({ ...refs, messages, isStreaming: false, contentHeight }),
        { initialProps: { messages: one, contentHeight: 2000 } },
      );
      vi.advanceTimersByTime(SMOOTH_FOLLOW_SETTLE_MS);
      userScrollsTo(1400);
      userScrollsTo(300);

      requestFollowBottom();
      // A height-only change (the same message list) while still far from
      // the end: left behind, but inside the hold the reader still counts as
      // at the end.
      scroller.scrollHeight = 2300;
      rerender({ messages: one, contentHeight: 2300 });
      // The next message is not theirs; it is followed all the same.
      scroller.scrollHeight = 2900;
      rerender({ messages: [1, 2], contentHeight: 2900 });
      expect(scroller.scrollTo).toHaveBeenLastCalledWith({ top: 2900, behavior: "smooth" });

      // After the hold, the same kind of change clears the pin: the reader
      // lands at the end (still pinned, and they do not scroll), a row then
      // grows by more than the window, and they are left behind for good, so
      // the next message is not followed. Inside the hold it was (above).
      vi.advanceTimersByTime(PIN_HOLD_MS + SMOOTH_FOLLOW_SETTLE_MS);
      userScrollsTo(2300);
      const two: readonly unknown[] = [1, 2];
      rerender({ messages: two, contentHeight: 2900 });
      vi.advanceTimersByTime(SMOOTH_FOLLOW_SETTLE_MS);
      scroller.scrollHeight = 3300;
      rerender({ messages: two, contentHeight: 3300 });
      const calls = scroller.scrollTo.mock.calls.length;
      scroller.scrollHeight = 3900;
      rerender({ messages: [1, 2, 3], contentHeight: 3900 });
      expect(scroller.scrollTo).toHaveBeenCalledTimes(calls);
    } finally {
      vi.useRealTimers();
    }
  });

  // The app placed the reader at a particular message: they are not at the
  // end any more, however they got there.
  it("stops following once the app has placed the reader somewhere", () => {
    vi.useFakeTimers();
    try {
      const { scroller, refs } = liveHarness();
      const { rerender } = renderHook(
        ({ messages }: { messages: readonly unknown[] }) =>
          useStickToBottom({ ...refs, messages, isStreaming: false }),
        { initialProps: { messages: [1] as readonly unknown[] } },
      );
      vi.advanceTimersByTime(SMOOTH_FOLLOW_SETTLE_MS);
      expect(scroller.scrollTo).toHaveBeenCalledTimes(1);

      requestHoldPosition();
      scroller.scrollTop = 200;
      scroller.scrollHeight = 2900;
      rerender({ messages: [1, 2] });
      expect(scroller.scrollTo).toHaveBeenCalledTimes(1);
    } finally {
      vi.useRealTimers();
    }
  });

  // The same reader, scrolling slowly: 120px up in 1px steps, then a row
  // near the viewport is measured for the first time and the height moves.
  // None of the steps is a "move" by itself; together they are.
  it("does not pull back a reader who scrolled up slowly", () => {
    vi.useFakeTimers();
    try {
      const { scroller, userScrollsTo, refs } = liveHarness();
      const one: readonly unknown[] = [1];
      const { rerender } = renderHook(
        ({ messages, contentHeight }: { messages: readonly unknown[]; contentHeight: number }) =>
          useStickToBottom({ ...refs, messages, isStreaming: false, contentHeight }),
        { initialProps: { messages: one, contentHeight: 2000 } },
      );
      vi.advanceTimersByTime(SMOOTH_FOLLOW_SETTLE_MS);
      userScrollsTo(1400);
      for (let top = 1399; top >= 1280; top--) userScrollsTo(top);
      const calls = scroller.scrollTo.mock.calls.length;
      scroller.scrollHeight = 2030;
      rerender({ messages: one, contentHeight: 2030 });
      expect(scroller.scrollTo).toHaveBeenCalledTimes(calls);
      // Nor does the next message carry them to the end.
      scroller.scrollHeight = 2400;
      rerender({ messages: [1, 2], contentHeight: 2400 });
      expect(scroller.scrollTo).toHaveBeenCalledTimes(calls);
    } finally {
      vi.useRealTimers();
    }
  });

  // Hidden (another pane or page laid over the chat) and shown again: a
  // hidden box forgets its scroll position, so the hook puts the reader back.
  describe("when the transcript is hidden and shown again", () => {
    function hiddenHarness() {
      const h = liveHarness();
      let observed: (() => void) | null = null;
      vi.stubGlobal(
        "ResizeObserver",
        class {
          constructor(cb: () => void) {
            observed = cb;
          }
          observe() {}
          disconnect() {}
        },
      );
      vi.stubGlobal("requestAnimationFrame", (cb: () => void) => {
        cb();
        return 0;
      });
      return {
        ...h,
        hide() {
          Object.assign(h.scroller, { scrollTop: 0, clientHeight: 0 });
          observed?.();
        },
        show() {
          Object.assign(h.scroller, { scrollTop: 0, clientHeight: 600 });
          observed?.();
        },
        /** The observer notices a pane that is already showing; the position is left as it is. */
        shown() {
          observed?.();
        },
      };
    }
    const mount = (refs: ReturnType<typeof hiddenHarness>["refs"]) =>
      renderHook(() => useStickToBottom({ ...refs, messages: [1], isStreaming: false }));

    it("returns a reader who had scrolled up to where they were", () => {
      vi.useFakeTimers();
      try {
        const { scroller, userScrollsTo, hide, show, refs } = hiddenHarness();
        mount(refs);
        userScrollsTo(1400);
        userScrollsTo(500);
        hide();
        show();
        expect(scroller.scrollTop).toBe(500);
      } finally {
        vi.useRealTimers();
        vi.unstubAllGlobals();
      }
    });

    it("returns a reader who was at the end to the end, even as it moves", () => {
      vi.useFakeTimers();
      try {
        const { scroller, userScrollsTo, hide, show, refs } = hiddenHarness();
        mount(refs);
        userScrollsTo(1400);
        hide();
        show();
        expect(scroller.scrollTop).toBe(2000);
        // Rows settle: something else nudges the position and the end moves.
        scroller.scrollTop = 1500;
        scroller.scrollHeight = 2400;
        vi.advanceTimersByTime(250);
        expect(scroller.scrollTop).toBe(2400);
      } finally {
        vi.useRealTimers();
        vi.unstubAllGlobals();
      }
    });

    it("leaves a reader the app placed in the same step where they were put", () => {
      vi.useFakeTimers();
      try {
        const { scroller, userScrollsTo, hide, shown, refs } = hiddenHarness();
        mount(refs);
        userScrollsTo(1400);
        userScrollsTo(500);
        hide();
        // The Board asks for a message: the pane is shown and scrolled there
        // before the observer has noticed the return. `shown()` fires the
        // observer without touching the position, so anything the hook does
        // to it (restoring 500, or going to the end) shows in the assertion.
        Object.assign(scroller, { clientHeight: 600 });
        requestHoldPosition();
        scroller.scrollTop = 900;
        shown();
        expect(scroller.scrollTop).toBe(900);
        vi.advanceTimersByTime(250);
        expect(scroller.scrollTop).toBe(900);
      } finally {
        vi.useRealTimers();
        vi.unstubAllGlobals();
      }
    });
  });

  // The scrolling element is shared between conversations: "scrolled up" in
  // the chat the reader left must not apply to the one they start next.
  it("starts another conversation's reader at the end", () => {
    vi.useFakeTimers();
    try {
      const { scroller, userScrollsTo, refs } = liveHarness();
      const { rerender } = renderHook(
        ({ messages, conversationKey }: { messages: readonly unknown[]; conversationKey: string }) =>
          useStickToBottom({ ...refs, messages, isStreaming: false, conversationKey }),
        { initialProps: { messages: [1] as readonly unknown[], conversationKey: "a" } },
      );
      vi.advanceTimersByTime(SMOOTH_FOLLOW_SETTLE_MS);
      userScrollsTo(1400);
      userScrollsTo(0);

      // Same conversation: left alone.
      scroller.scrollHeight = 2700;
      rerender({ messages: [1, 2], conversationKey: "a" });
      expect(scroller.scrollTo).toHaveBeenCalledTimes(1);

      // A new chat whose first message is long.
      scroller.scrollHeight = 1500;
      scroller.scrollTop = 0;
      rerender({ messages: [9, 10], conversationKey: "b" });
      expect(scroller.scrollTo).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  // A hidden transcript (another pane or page laid over it) measures as
  // nothing, which looks like "at the end". No decision is taken on it.
  it("decides nothing while the transcript is hidden", () => {
    vi.useFakeTimers();
    try {
      const { scroller, userScrollsTo, refs } = liveHarness();
      const visible = { value: true };
      (scroller as unknown as { checkVisibility: () => boolean }).checkVisibility = () => visible.value;
      const { rerender } = renderHook(
        ({ messages }: { messages: readonly unknown[] }) =>
          useStickToBottom({ ...refs, messages, isStreaming: true }),
        { initialProps: { messages: [1] as readonly unknown[] } },
      );
      userScrollsTo(1400);
      userScrollsTo(500);

      visible.value = false;
      const shown = { ...scroller };
      Object.assign(scroller, { scrollTop: 0, scrollHeight: 0, clientHeight: 0 });
      rerender({ messages: [1, 2] });
      expect(scroller.scrollTo).toHaveBeenCalledTimes(1);

      // Back in view, still streaming: the reader who scrolled up stays put.
      visible.value = true;
      Object.assign(scroller, { scrollTop: 500, scrollHeight: shown.scrollHeight + 400, clientHeight: 600 });
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
