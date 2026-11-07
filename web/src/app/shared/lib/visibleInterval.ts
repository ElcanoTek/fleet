// A polling interval that rests while the tab is hidden (#2004).
//
// A backgrounded tab's setInterval keeps firing (throttled, but firing), so
// every dashboard, admin page and viewer left open in a background tab kept
// polling the server for a screen nobody was looking at. startVisibleInterval
// skips a tick while the document is hidden and instead runs `fn` once when
// the tab comes back into view, so the page is current the moment it is seen
// again. It does not run `fn` on start: callers do their own first load.
//
// "Hidden" is visibilityState === "hidden" (not `document.hidden`), the same
// check the rest of the app uses, so a test that stubs one stubs both.

function isHidden(): boolean {
  return typeof document !== "undefined" && document.visibilityState === "hidden";
}

/** Starts the interval; returns the cleanup. */
export function startVisibleInterval(fn: () => void, ms: number): () => void {
  const tick = () => {
    if (!isHidden()) fn();
  };
  const onVisibility = () => {
    if (!isHidden()) fn();
  };
  const id = window.setInterval(tick, ms);
  document.addEventListener("visibilitychange", onVisibility);
  return () => {
    window.clearInterval(id);
    document.removeEventListener("visibilitychange", onVisibility);
  };
}
