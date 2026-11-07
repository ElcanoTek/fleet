// useVisibleInterval — startVisibleInterval (shared/lib/visibleInterval) as a
// hook (#2004): runs `fn` every `ms` while `enabled` and the tab is visible,
// and once when the tab comes back into view. It never runs `fn` on mount.
//
// `fn` is read through a ref, so a new closure each render neither re-arms
// the timer nor runs a stale one; changing `ms` or `enabled` re-arms it.

import { useEffect, useRef } from "react";
import { startVisibleInterval } from "@/app/shared/lib/visibleInterval";

export function useVisibleInterval(fn: () => void, ms: number, enabled = true): void {
  const fnRef = useRef(fn);
  useEffect(() => {
    fnRef.current = fn;
  });
  useEffect(() => {
    if (!enabled || !(ms > 0)) return;
    return startVisibleInterval(() => fnRef.current(), ms);
  }, [ms, enabled]);
}
