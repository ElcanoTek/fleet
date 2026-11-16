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
  /**
   * true for the one re-check made when a smooth follow of ours lands, on
   * behalf of changes that arrived while it was under way. The reader was
   * being carried to the end throughout (had they scrolled up, `wasPinned`
   * would be false), so they are carried the rest of the way however much
   * the content grew meanwhile; the idle window is for a reader at rest.
   */
  afterOwnFollow?: boolean;
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
  afterOwnFollow = false,
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
  const carriedPastWindow = wasPinned === true && (!restingRemeasure || afterOwnFollow);
  // We are already carrying this reader to the end (a smooth follow of ours
  // is under way and they have not scrolled up since). Until it lands, the
  // distance is the animation's, not theirs: a row that re-measures one frame
  // after a tall message arrived would otherwise find them "far from the end"
  // and count them as having scrolled away, with nothing left to bring them
  // the rest of the way. Wait for the landing and judge then.
  if (behavior === "smooth" && smoothFollowInFlight && wasPinned === true) {
    return { follow: false, reason: "smooth-follow-in-flight" };
  }
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

// The reader asked to be at the end: they sent a message, or pressed "Jump to
// latest". Distance cannot say this. Their own long message moves the end
// away by more than any follow window, and the jump button's smooth scroll is
// still on its way when the next change arrives, so the callers that know
// say so, and every mounted transcript counts its reader as at the end again.
const followRequests = new Set<() => void>();
export function requestFollowBottom(): void {
  for (const pin of followRequests) pin();
}

/** How long after a request to be at the end the pin survives a change that outran the scroll there. */
export const PIN_HOLD_MS = 1500;
/** How long after the app places the reader before their position is read back. */
export const HOLD_SETTLE_MS = 400;

// The opposite request: the app has just put the reader at a particular
// message (the Board asking for the approval that is waiting). They are no
// longer at the end, and nothing pending (the return to the end or to their
// old place when the transcript is shown again) may move them.
const holdRequests = new Set<() => void>();
export function requestHoldPosition(): void {
  for (const hold of holdRequests) hold();
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
  /**
   * Which conversation the transcript shows. The scrolling element is shared
   * between conversations, so "the reader scrolled up" would otherwise carry
   * from the chat they left into the one they open or start next.
   */
  conversationKey?: string | null;
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
  conversationKey,
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
  const runRef = useRef<(heightOnly: boolean, baseline: number | null, afterOwnFollow?: boolean) => void>(
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
  const conversationKeyRef = useRef(conversationKey);
  // Until this time a "left behind" decision does not clear the pin. A
  // request to be at the end (Jump to latest) scrolls there smoothly; a row
  // measured for the first time on the way is a change the reader is not yet
  // level with, and must not be read as them having scrolled away.
  const pinHeldUntilRef = useRef(0);
  // Where the reader was the last time the transcript was on screen, and
  // whether it has been hidden since. A hidden box (display: none) forgets
  // its scroll position, so it is put back when the box returns.
  const lastVisibleTopRef = useRef<number | null>(null);
  const wasHiddenRef = useRef(false);
  const skipRestoreRef = useRef(false);
  const visibilityObserverRef = useRef<ResizeObserver | null>(null);

  useEffect(() => {
    const pin = () => {
      pinnedRef.current = true;
      lastScrollRef.current = null;
      pinHeldUntilRef.current = Date.now() + PIN_HOLD_MS;
    };
    let holdCheck: number | null = null;
    const hold = () => {
      pinnedRef.current = false;
      pinHeldUntilRef.current = 0;
      lastScrollRef.current = null;
      // The place they were put may be the end itself (the message asked
      // for is the last one): once the jump has settled, read it back, or a
      // reader sitting at the end would count as scrolled away until they
      // next moved.
      if (holdCheck !== null) window.clearTimeout(holdCheck);
      holdCheck = window.setTimeout(() => {
        holdCheck = null;
        const el = listenedRef.current;
        if (el && el.clientHeight > 0 && el.scrollHeight - el.scrollTop - el.clientHeight <= FOLLOW_BOTTOM_EPSILON_PX) {
          pinnedRef.current = true;
        }
      }, HOLD_SETTLE_MS);
      // Shown in the same step as the jump: the observer has not noticed
      // the return yet, and must not put the reader back when it does.
      skipRestoreRef.current = wasHiddenRef.current;
    };
    followRequests.add(pin);
    holdRequests.add(hold);
    return () => {
      followRequests.delete(pin);
      holdRequests.delete(hold);
      if (holdCheck !== null) window.clearTimeout(holdCheck);
    };
  }, []);

  // Mount-scoped: the settle timer outlives individual effect runs on purpose
  // (clearing it per run would release the in-flight guard early, which is the
  // guard's whole job), so it is drained once, on unmount.
  useEffect(
    () => () => {
      if (settleTimerRef.current !== null) {
        window.clearTimeout(settleTimerRef.current);
        settleTimerRef.current = null;
      }
      // The timer is what would have cleared these. Left set (React's strict
      // mode unmounts and remounts once in development), every later follow
      // would be told one was still in flight, for good.
      smoothFollowInFlightRef.current = false;
      recheckAfterSettleRef.current = null;
      listenedRef.current?.removeEventListener("scroll", onScrollRef.current);
      listenedRef.current = null;
      visibilityObserverRef.current?.disconnect();
      visibilityObserverRef.current = null;
    },
    [],
  );

  useEffect(() => {
    isStreamingRef.current = isStreaming;
    const last = lastTriggerRef.current;
    const heightOnly =
      last !== null && last.messages === messages && last.isStreaming === isStreaming;
    lastTriggerRef.current = { messages, isStreaming };
    // Another conversation: its reader starts at the end, wherever the last
    // one was left.
    if (conversationKeyRef.current !== conversationKey) {
      conversationKeyRef.current = conversationKey;
      pinnedRef.current = true;
      lastScrollRef.current = null;
    }
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
      lastVisibleTopRef.current = null;
      wasHiddenRef.current = false;
      visibilityObserverRef.current?.disconnect();
      visibilityObserverRef.current = null;
      if (liveScroller && typeof ResizeObserver !== "undefined") {
        // Hidden and shown again (the Dashboard's Board pane below lg, a
        // project page laid over the chat): the reader goes back to where
        // they were, or to the end if that is where they were.
        visibilityObserverRef.current = new ResizeObserver(() => {
          const el = listenedRef.current;
          if (!el) return;
          if (el.clientHeight === 0) {
            wasHiddenRef.current = true;
            return;
          }
          if (!wasHiddenRef.current) {
            // A hold asked for while the transcript was on screen has
            // nothing to skip.
            skipRestoreRef.current = false;
            return;
          }
          wasHiddenRef.current = false;
          lastScrollRef.current = null;
          // The app placed the reader itself in the same step (see
          // requestHoldPosition): leave them there.
          if (skipRestoreRef.current) {
            skipRestoreRef.current = false;
            lastVisibleTopRef.current = el.scrollTop;
            return;
          }
          const remembered = lastVisibleTopRef.current;
          if (!pinnedRef.current) {
            if (remembered !== null) el.scrollTop = remembered;
            return;
          }
          // At the end before, at the end now. The rows are measured again
          // over the next frames and the end moves with them, so go there
          // again once they have, and do not let that re-measuring count as
          // the reader having been left behind. It stops by itself the
          // moment they stop counting as at the end: the reader scrolling up
          // or the app placing them (requestHoldPosition) both clear the pin.
          // Nothing else is taken as a reason to stop: the browser and the
          // virtualizer adjust the position themselves while rows settle.
          pinHeldUntilRef.current = Date.now() + PIN_HOLD_MS;
          const toEnd = () => {
            if (listenedRef.current !== el || !pinnedRef.current || el.clientHeight === 0) return;
            el.scrollTop = el.scrollHeight;
          };
          toEnd();
          requestAnimationFrame(() => {
            toEnd();
            requestAnimationFrame(toEnd);
          });
          window.setTimeout(toEnd, 250);
        });
        visibilityObserverRef.current.observe(liveScroller);
      }
      onScrollRef.current = () => {
        const el = listenedRef.current;
        if (!el) return;
        // A hidden box reports 0 / 0 / 0, which would read as "at the end".
        // Events between a hide and the observer above noticing the return
        // are the browser resetting the position, not the reader.
        if (el.clientHeight === 0) {
          if (visibilityObserverRef.current) wasHiddenRef.current = true;
          return;
        }
        if (wasHiddenRef.current) return;
        const top = el.scrollTop;
        lastVisibleTopRef.current = top;
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
    runRef.current = (runHeightOnly, baseline, afterOwnFollow = false) => {
      const sentinel = streamEndRef.current;
      if (!sentinel) return;
      const scroller = conversationRef.current;
      // Hidden (the Dashboard's Board pane, a project page laid over the
      // chat): a hidden box measures 0 / 0 / 0, which reads as "at the end".
      // Deciding on that would mark a reader who had scrolled up as pinned
      // and carry them to the end when they came back. Wait until it shows.
      if (scroller && typeof scroller.checkVisibility === "function" && !scroller.checkVisibility()) {
        return;
      }
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
        afterOwnFollow,
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
        if (decision.reason === "user-scrolled-away" && Date.now() >= pinHeldUntilRef.current) {
          pinnedRef.current = false;
        }
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
              true,
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
  }, [messages, isStreaming, contentHeight, conversationKey, conversationRef, streamEndRef]);
}
