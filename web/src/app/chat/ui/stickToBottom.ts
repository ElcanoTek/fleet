// stickToBottom — the transcript's follow-the-bottom behavior, as a pure
// decision function plus the thin hook that acts on it.
//
// Dynamic rows can change height while a scroll is in flight. This hook avoids
// redundant follow requests, waits for a smooth correction to settle, and
// honors reduced motion. It only reacts to content changes, not scroll events:
// a scroll is recorded (was that the reader moving up, or coming back to the
// end?) and nothing is ever scheduled from one.
//
// "Content changes" means two things: the message list changed, or the
// transcript's measured height changed. The second matters because a virtual
// row is measured AFTER the render that changed it: when a reply finishes, its
// action row mounts and the row is re-measured a frame or two after the last
// message update, so the transcript grew with nothing left to trigger a
// follow, and the newest reply's actions ended up under the composer. A growth
// that arrives while a smooth follow is still animating is not dropped either:
// it is re-checked once, when that follow settles.
//
// A height-only change at rest is held to a stricter test than a new message:
// it follows only a reader who was pinned to the bottom before the growth.
// "Pinned" is what the scroll record says, not a guess from distances: a
// reader who has not scrolled up stays pinned when their own long message
// pushes the end far away, and when a row shrinks and regrows between two
// looks — both of which a distance test read as "scrolled away".
// Rows are also re-measured as they scroll into view, so without that test a
// reader who scrolled up a little to re-read was pulled back down by their own
// scrolling.
//
// These guards do not fix a renderer that repeatedly remounts its contents.
// In particular, the teammate-branch image shake (#11) came from unstable
// Markdown component identities resetting failed images on each measurement;
// AssistantContent preserves those identities so that resize loop never starts.

import { useEffect, useRef } from "react";
import type { RefObject } from "react";

/**
 * The height change below which the transcript is considered NOT to have
 * grown. Chosen, not tuned:
 *
 *   - The smallest genuine growth the transcript can add is one line of text
 *     — ~19px at the transcript's smallest type (0.78rem / 1.55 leading) —
 *     and the smallest inter-row gap is 20px. So 4px is far below anything a
 *     real content change produces.
 *   - The noise it has to swallow is dynamic re-measurement of the same
 *     content: `getBoundingClientRect` returns fractional CSS pixels, and at
 *     a fractional device pixel ratio (1.25, 1.5, 2) a re-measure of an
 *     unchanged row can differ by a fraction of a pixel — accumulating to a
 *     pixel or two across the handful of rows near the viewport.
 *
 * 4px sits an order of magnitude below real growth and comfortably above
 * measurement noise, which is the only property that matters.
 */
export const FOLLOW_BOTTOM_EPSILON_PX = 4;

/**
 * How far from the bottom the reader may be and still get pulled along.
 * These preserve the thresholds the transcript has always used: a wider
 * window mid-stream (text is arriving, so the reader expects to be carried),
 * a tighter one at rest (a deliberate scroll up must not be undone).
 */
export const STREAMING_FOLLOW_WINDOW_PX = 240;
export const IDLE_FOLLOW_WINDOW_PX = 160;

/**
 * How long a smooth follow is treated as in flight. A bounded timer rather
 * than a `scrollend` listener on purpose: `scrollend` is not available in
 * every engine we serve, and a smooth scroll the user interrupts mid-animation
 * does not reliably fire it anywhere — a timer always releases.
 */
export const SMOOTH_FOLLOW_SETTLE_MS = 500;

export type ScrollMetrics = {
  scrollTop: number;
  scrollHeight: number;
  clientHeight: number;
};

export type FollowBottomSkipReason =
  /** The reader is deliberately reading further up; never yank them down. */
  | "user-scrolled-away"
  /** A smooth follow is still animating — a second one is the oscillation. */
  | "smooth-follow-in-flight"
  /** Already at the bottom and nothing grew beyond measurement noise. */
  | "pinned-and-unchanged";

export type FollowBottomDecision =
  | { follow: true; behavior: "auto" | "smooth" }
  | { follow: false; reason: FollowBottomSkipReason };

export type FollowBottomInput = {
  /** Live metrics of the scroll container, or null when it is not mounted. */
  metrics: ScrollMetrics | null;
  isStreaming: boolean;
  /** scrollHeight observed the last time this ran; null on the first run. */
  previousScrollHeight: number | null;
  /** true while a smooth follow issued earlier has not yet settled. */
  smoothFollowInFlight: boolean;
  prefersReducedMotion: boolean;
  /**
   * true when only the measured height changed since the last run (the
   * message list and the streaming flag are the same). At rest such a run
   * follows only a reader who was pinned before the growth.
   */
  heightOnly?: boolean;
  /**
   * Whether the reader was at the end before this change: they were there the
   * last time we looked, or we sent them there, and they have not scrolled up
   * since (the hook tracks this from scroll events). When given it replaces
   * the two guesses below that stand in for it — "within the follow window"
   * and "the growth explains the distance" — which both misread a reader who
   * never moved: a long message of their own pushed the end further than the
   * window, or a row shrank and regrew between two looks so the growth
   * measured as nothing. Omitted, the guesses apply as before.
   */
  wasPinned?: boolean;
};

/**
 * decideFollowBottom is the whole policy, as a pure function of numbers — so
 * it can be tested without a layout engine (jsdom performs none).
 */
export function decideFollowBottom({
  metrics,
  isStreaming,
  previousScrollHeight,
  smoothFollowInFlight,
  prefersReducedMotion,
  heightOnly = false,
  wasPinned,
}: FollowBottomInput): FollowBottomDecision {
  // Instant while streaming (deltas arrive faster than an animation can
  // finish) and instant under reduced motion; smooth only for the one-off
  // correction at rest.
  const behavior: "auto" | "smooth" =
    isStreaming || prefersReducedMotion ? "auto" : "smooth";

  // No scroll container yet (first paint): there is no measurement to
  // oscillate against, so honor the follow and let the next run establish a
  // baseline.
  if (!metrics) return { follow: true, behavior };

  const distanceFromBottom =
    metrics.scrollHeight - metrics.scrollTop - metrics.clientHeight;
  const followWindow = isStreaming
    ? STREAMING_FOLLOW_WINDOW_PX
    : IDLE_FOLLOW_WINDOW_PX;
  const restingRemeasure = heightOnly && !isStreaming;
  // A reader known to be at the end is carried however far a new message or
  // streamed text moved it: their own long message must not count as them
  // scrolling away. A row that merely changed height at rest keeps the
  // window even for them, so opening a tall panel in the last reply does not
  // scroll the control they just pressed out of view.
  const carriedPastWindow = wasPinned === true && !restingRemeasure;
  if (distanceFromBottom > followWindow && !carriedPastWindow) {
    return { follow: false, reason: "user-scrolled-away" };
  }

  if (behavior === "smooth" && smoothFollowInFlight) {
    return { follow: false, reason: "smooth-follow-in-flight" };
  }

  // A re-measurement with no new message, at rest. The idle window above is
  // for a NEW message, where carrying a reader who is near the end is what
  // they expect. A row that merely changed height earns a follow only if the
  // reader was at the bottom before it grew: take the growth back out of the
  // distance and see whether they were pinned. Anyone further up scrolled
  // there on purpose, and scrolling itself re-measures rows.
  if (restingRemeasure) {
    let pinnedBefore = wasPinned;
    if (pinnedBefore === undefined) {
      const growth =
        previousScrollHeight === null
          ? 0
          : Math.max(0, metrics.scrollHeight - previousScrollHeight);
      pinnedBefore = distanceFromBottom - growth <= FOLLOW_BOTTOM_EPSILON_PX;
    }
    if (!pinnedBefore) {
      return { follow: false, reason: "user-scrolled-away" };
    }
  }

  // The loop breaker. Pinned at the bottom AND the content did not really
  // grow: another scroll can only re-run measurement and invite the next
  // pixel of jitter. `previousScrollHeight === null` (first run) always
  // follows, since there is no baseline to call unchanged.
  if (previousScrollHeight !== null) {
    const growth = metrics.scrollHeight - previousScrollHeight;
    if (
      Math.abs(growth) < FOLLOW_BOTTOM_EPSILON_PX &&
      distanceFromBottom <= FOLLOW_BOTTOM_EPSILON_PX
    ) {
      return { follow: false, reason: "pinned-and-unchanged" };
    }
  }

  return { follow: true, behavior };
}

/**
 * prefersReducedMotion reads the media query defensively: `matchMedia` is
 * absent in SSR and stubbed differently across test environments, and a
 * missing preference must never be reported as "reduce".
 */
export function prefersReducedMotion(): boolean {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") {
    return false;
  }
  try {
    return window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  } catch {
    return false;
  }
}

export type UseStickToBottomInput = {
  /** The scrolling transcript container. */
  conversationRef: RefObject<HTMLElement | null>;
  /** The zero-height sentinel at the very end of the transcript. */
  streamEndRef: RefObject<HTMLDivElement | null>;
  /**
   * Any value whose identity changes when the transcript's content changes —
   * in practice the message list. It is the effect's trigger, nothing reads
   * inside it.
   */
  messages: readonly unknown[];
  isStreaming: boolean;
  /**
   * The transcript's measured content height (the virtualizer's total size).
   * A second trigger beside `messages`: rows are re-measured after the render
   * that changed them, so the height can change with the message list
   * untouched. Like `messages`, nothing reads it inside the effect.
   */
  contentHeight?: number;
};

/**
 * useStickToBottom keeps the transcript pinned to its end, applying
 * decideFollowBottom on every content change. It performs at most one scroll
 * per run and schedules nothing from a scroll handler, so the only thing that
 * can trigger a follow is a content change — a scroll it issued itself cannot
 * come back around as a second follow.
 */
export function useStickToBottom({
  conversationRef,
  streamEndRef,
  messages,
  isStreaming,
  contentHeight,
}: UseStickToBottomInput): void {
  const previousScrollHeightRef = useRef<number | null>(null);
  const smoothFollowInFlightRef = useRef(false);
  const settleTimerRef = useRef<number | null>(null);
  // Set when a run was skipped only because a smooth follow was animating;
  // the settle timer then runs the decision once more, so growth that landed
  // mid-animation (which the animation's fixed target cannot see) is caught.
  // It remembers whether every skipped run was height-only, and the height
  // from before the first of them: the re-check has to measure the growth the
  // animation missed, not the nothing that changed since the last skip.
  const recheckAfterSettleRef = useRef<{
    heightOnly: boolean;
    baseline: number | null;
  } | null>(null);
  const isStreamingRef = useRef(isStreaming);
  const lastTriggerRef = useRef<{
    messages: readonly unknown[];
    isStreaming: boolean;
  } | null>(null);
  const runRef = useRef<(heightOnly: boolean, baseline: number | null) => void>(
    () => {},
  );
  // Whether the reader is at the end: true until they scroll up, true again
  // when they come back down or we send them there. Kept from scroll events
  // (record only — nothing is scheduled from the handler) because distance
  // alone cannot tell "scrolled up" from "the end moved away".
  const pinnedRef = useRef(true);
  const lastScrollRef = useRef<{ top: number; distance: number } | null>(null);
  const listenedRef = useRef<HTMLElement | null>(null);
  const onScrollRef = useRef<() => void>(() => {});

  // Mount-scoped: the settle timer outlives individual effect runs on purpose
  // (clearing it per run would release the in-flight guard early, which is the
  // guard's whole job), so it is drained once, on unmount.
  useEffect(
    () => () => {
      if (settleTimerRef.current !== null) {
        window.clearTimeout(settleTimerRef.current);
        settleTimerRef.current = null;
      }
      listenedRef.current?.removeEventListener("scroll", onScrollRef.current);
      listenedRef.current = null;
    },
    [],
  );

  useEffect(() => {
    isStreamingRef.current = isStreaming;
    const last = lastTriggerRef.current;
    const heightOnly =
      last !== null && last.messages === messages && last.isStreaming === isStreaming;
    lastTriggerRef.current = { messages, isStreaming };
    // The scroller can be replaced (another conversation's transcript), so
    // the listener follows whichever element the ref holds now.
    const liveScroller = conversationRef.current;
    if (
      liveScroller !== listenedRef.current &&
      (!liveScroller || typeof liveScroller.addEventListener === "function")
    ) {
      listenedRef.current?.removeEventListener("scroll", onScrollRef.current);
      listenedRef.current = liveScroller;
      lastScrollRef.current = null;
      pinnedRef.current = true;
      onScrollRef.current = () => {
        const el = listenedRef.current;
        if (!el) return;
        const top = el.scrollTop;
        const distance = el.scrollHeight - top - el.clientHeight;
        const last = lastScrollRef.current;
        lastScrollRef.current = { top, distance };
        if (distance <= FOLLOW_BOTTOM_EPSILON_PX) {
          pinnedRef.current = true;
        } else if (last && top < last.top - 1 && distance > last.distance + 1) {
          // Moved up and away from the end. Our own follows only ever move
          // down, and a row above changing height shifts the position without
          // changing the distance, so this is the reader.
          pinnedRef.current = false;
        }
      };
      liveScroller?.addEventListener("scroll", onScrollRef.current, { passive: true });
    }
    runRef.current = (runHeightOnly, baseline) => {
      const sentinel = streamEndRef.current;
      if (!sentinel) return;
      const scroller = conversationRef.current;
      const metrics: ScrollMetrics | null = scroller
        ? {
            scrollTop: scroller.scrollTop,
            scrollHeight: scroller.scrollHeight,
            clientHeight: scroller.clientHeight,
          }
        : null;

      const decision = decideFollowBottom({
        metrics,
        isStreaming: isStreamingRef.current,
        previousScrollHeight: baseline,
        smoothFollowInFlight: smoothFollowInFlightRef.current,
        prefersReducedMotion: prefersReducedMotion(),
        heightOnly: runHeightOnly,
        // Only where the scroll listener could be attached; otherwise the
        // decision falls back to its own estimate.
        wasPinned: listenedRef.current ? pinnedRef.current : undefined,
      });

      // Record the baseline whether or not we scroll: scrollIntoView does not
      // change scrollHeight, so the value read above is the one the next run
      // must compare against.
      if (metrics) previousScrollHeightRef.current = metrics.scrollHeight;

      if (!decision.follow) {
        // Left behind by a change we chose not to follow: from here on they
        // count as reading further up, until they return to the end.
        if (decision.reason === "user-scrolled-away") pinnedRef.current = false;
        if (decision.reason === "smooth-follow-in-flight") {
          const pending = recheckAfterSettleRef.current;
          recheckAfterSettleRef.current = {
            heightOnly: (pending ? pending.heightOnly : true) && runHeightOnly,
            baseline: pending ? pending.baseline : baseline,
          };
        }
        return;
      }

      pinnedRef.current = true;
      if (decision.behavior === "smooth") {
        smoothFollowInFlightRef.current = true;
        if (settleTimerRef.current !== null) {
          window.clearTimeout(settleTimerRef.current);
        }
        settleTimerRef.current = window.setTimeout(() => {
          settleTimerRef.current = null;
          smoothFollowInFlightRef.current = false;
          const pending = recheckAfterSettleRef.current;
          if (pending) {
            recheckAfterSettleRef.current = null;
            runRef.current(
              pending.heightOnly,
              pending.heightOnly ? pending.baseline : previousScrollHeightRef.current,
            );
          }
        }, SMOOTH_FOLLOW_SETTLE_MS);
      }

      // Follow to the scroller's true end, not to the sentinel: the transcript
      // keeps a bottom padding below the sentinel, so a sentinel-aligned
      // follow rests that padding short of the end — the newest reply's
      // action row then sits flush against the composer, and "pinned" (which
      // decideFollowBottom measures from the true end) never holds. The
      // sentinel remains the fallback where the scroller cannot be driven.
      if (scroller && typeof scroller.scrollTo === "function") {
        scroller.scrollTo({ top: scroller.scrollHeight, behavior: decision.behavior });
      } else {
        sentinel.scrollIntoView({ block: "end", behavior: decision.behavior });
      }
    };
    runRef.current(heightOnly, previousScrollHeightRef.current);
  }, [messages, isStreaming, contentHeight, conversationRef, streamEndRef]);
}
