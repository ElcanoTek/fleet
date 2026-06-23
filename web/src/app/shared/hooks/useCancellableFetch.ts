"use client";

import { useCallback, useEffect, useRef, useState } from "react";

// useCancellableFetch — the single audited place where a setState lands after
// an await inside an effect. Several call sites (the MCP catalog, the model
// picker, the log viewer) all share the same shape: kick off one async fetch
// when some input changes, swap in the result, capture any error, and guard
// against a state update racing an unmount or a superseding fetch. Centralizing
// that here keeps the cancelled-ref bookkeeping in one place and lets the call
// sites drop their bespoke `react-hooks/set-state-in-effect` disables.
//
// Why `loading` is lazy-initialized to `enabled` and never set true in the
// effect's synchronous phase: a synchronous `setLoading(true)` at the top of an
// effect trips react-hooks/set-state-in-effect (it cascades a render straight
// off the effect body). Instead `loading` starts true whenever the hook is
// enabled, and is only flipped *false* once a fetch settles. Manual reloads
// flip it true from inside an async callback (an event-driven update, not the
// effect's sync phase), so the rule stays satisfied without suppression.

export type UseCancellableFetch<T> = {
  data: T | null;
  loading: boolean;
  error: string | null;
  // reload re-runs the fetcher imperatively (button, parent event). It resolves
  // when the fetch settles; a fetch superseded by a newer reload/unmount is
  // dropped without touching state.
  reload: () => Promise<void>;
};

export function useCancellableFetch<T>(
  fetcher: () => Promise<T>,
  deps: ReadonlyArray<unknown>,
  options: { enabled?: boolean } = {},
): UseCancellableFetch<T> {
  const enabled = options.enabled ?? true;

  const [data, setData] = useState<T | null>(null);
  // Lazy-init to `enabled`: we begin in the loading state the very first render
  // so callers don't briefly flash an empty/error view before the mount fetch
  // runs — and we never have to set it true synchronously inside the effect.
  const [loading, setLoading] = useState(enabled);
  const [error, setError] = useState<string | null>(null);

  // Bumped on every run; the in-flight closure compares against it so a stale
  // fetch (superseded by a newer reload, a dep change, or unmount) is ignored.
  const runIdRef = useRef(0);
  // Set false on unmount so even the latest in-flight fetch can't setState
  // after the component is gone.
  const mountedRef = useRef(true);
  // Keep the latest fetcher reachable without forcing callers to memoize it.
  // The ref is written in an effect (never during render) so it doesn't trip
  // react-hooks/refs; the deferred fetch kickoff below always runs after this
  // commit, so `run` sees the current fetcher.
  const fetcherRef = useRef(fetcher);
  useEffect(() => {
    fetcherRef.current = fetcher;
  });
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  const run = useCallback(async () => {
    const runId = ++runIdRef.current;
    // Setting loading true here is safe for the rule: `run` is invoked as an
    // async callback (from the effect's deferred kickoff or an imperative
    // reload), not synchronously in an effect body. The guard keeps a
    // superseded run from clobbering a newer one's loading state.
    setLoading(true);
    setError(null);
    try {
      const result = await fetcherRef.current();
      if (!mountedRef.current || runId !== runIdRef.current) return;
      setData(result);
    } catch (err) {
      if (!mountedRef.current || runId !== runIdRef.current) return;
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      if (mountedRef.current && runId === runIdRef.current) setLoading(false);
    }
  }, []);

  // Mount / dep-change fetch. The whole effect body is deferred to a microtask
  // so neither the `setLoading(true)` inside `run` nor the disabled-branch
  // `setLoading(false)` ever executes in the effect's synchronous phase. When
  // disabled we cancel any in-flight run and settle the loading flag instead
  // of fetching.
  //
  // The dep array spreads caller-provided `deps`, which ESLint cannot verify
  // statically — this is the inherent, documented seam of a generic fetch
  // hook, and the single remaining hooks-lint suppression in this file.
  useEffect(() => {
    let cancelled = false;
    queueMicrotask(() => {
      if (cancelled) return;
      if (!enabled) {
        // Supersede any in-flight run and clear the initial loading state.
        runIdRef.current++;
        if (mountedRef.current) setLoading(false);
        return;
      }
      void run();
    });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled, run, ...deps]);

  return { data, loading, error, reload: run };
}
