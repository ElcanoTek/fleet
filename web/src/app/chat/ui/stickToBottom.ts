"use client";

// stickToBottom — the transcript's follow-the-bottom behavior, as a pure
// decision function plus the thin hook that acts on it.
//
// Why this is not just `scrollIntoView` in an effect (QA finding #11). A
// teammate-branched chat that had contained images and tool output shook
// vertically — but only when scrolled to the very end, and it survived a
// reload. The mechanism is a feedback loop, not a data bug:
//
//   1. the transcript is virtualized with DYNAMIC measurement, so a row's
//      measured height feeds the spacer that feeds the scroller's
//      scrollHeight;
//   2. residue from the transcript-only branch copy (a user turn whose images
//      were stripped) rendered an empty bubble whose height came from padding
//      and line-box rounding rather than content;
//   3. pinned at the bottom, a follow-scroll re-runs measurement, the total
//      size moves by a pixel or two, and the follow fires again — with a
//      `behavior: "smooth"` scroll still animating into a container that is
//      re-measuring underneath it.
//
// The data half is fixed server-side (a branch copy no longer writes a row it
// has emptied) and the residue in branches that ALREADY exist is dropped by
// messageHasRenderableContent. But neither closes the loop: any future element
// with an unstable height re-opens it. So the loop itself is guarded here, by
// three rules:
//
//   - **Nothing moved, so do not scroll.** A follow only fires when the
//     content actually grew past FOLLOW_BOTTOM_EPSILON_PX, or when the reader
//     is genuinely off the bottom. Sub-pixel and few-pixel re-measure jitter
//     is ignored instead of answered with another scroll.
//   - **No re-entrant smooth follow.** A smooth scroll is asynchronous and
//     animates over many frames; issuing a second one while the first is still
//     running is what turns a one-off correction into an oscillation. The
//     second is skipped until the first has had time to settle.
//   - **Reduced motion means instant.** A smooth follow is the loop's motor,
//     and globals.css's `scroll-behavior: auto !important` under
//     `prefers-reduced-motion` does NOT reach it: an explicit `behavior` in
//     scrollIntoView's options overrides the computed scroll-behavior, so the
//     preference has to be honored here in JS.

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
  if (distanceFromBottom > followWindow) {
    return { follow: false, reason: "user-scrolled-away" };
  }

  if (behavior === "smooth" && smoothFollowInFlight) {
    return { follow: false, reason: "smooth-follow-in-flight" };
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
}: UseStickToBottomInput): void {
  const previousScrollHeightRef = useRef<number | null>(null);
  const smoothFollowInFlightRef = useRef(false);
  const settleTimerRef = useRef<number | null>(null);

  // Mount-scoped: the settle timer outlives individual effect runs on purpose
  // (clearing it per run would release the in-flight guard early, which is the
  // guard's whole job), so it is drained once, on unmount.
  useEffect(
    () => () => {
      if (settleTimerRef.current !== null) {
        window.clearTimeout(settleTimerRef.current);
        settleTimerRef.current = null;
      }
    },
    [],
  );

  useEffect(() => {
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
      isStreaming,
      previousScrollHeight: previousScrollHeightRef.current,
      smoothFollowInFlight: smoothFollowInFlightRef.current,
      prefersReducedMotion: prefersReducedMotion(),
    });

    // Record the baseline whether or not we scroll: scrollIntoView does not
    // change scrollHeight, so the value read above is the one the next run
    // must compare against.
    if (metrics) previousScrollHeightRef.current = metrics.scrollHeight;

    if (!decision.follow) return;

    if (decision.behavior === "smooth") {
      smoothFollowInFlightRef.current = true;
      if (settleTimerRef.current !== null) {
        window.clearTimeout(settleTimerRef.current);
      }
      settleTimerRef.current = window.setTimeout(() => {
        settleTimerRef.current = null;
        smoothFollowInFlightRef.current = false;
      }, SMOOTH_FOLLOW_SETTLE_MS);
    }

    sentinel.scrollIntoView({ block: "end", behavior: decision.behavior });
  }, [messages, isStreaming, conversationRef, streamEndRef]);
}
