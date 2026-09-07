"use client";

import { useEffect, useState } from "react";

// useIsAdmin — client-side admin visibility for the settings sub-nav and the
// /settings/admin/* pages. There is no member-visible "am I an admin" read
// (GET /api/session returns only { email }), so this probes one admin-gated
// endpoint and folds the status:
//
//   200 / 501  → admin (501 = admin-gated handler reached, service unwired —
//                the mock/test mode; the Features panel shows its own
//                "unavailable" state)
//   403        → not an admin
//   401        → not signed in (the pages' own fetches handle the redirect)
//
// This is VISIBILITY only — authorization stays server-side: every /api/admin
// call is independently rejected upstream for non-admins, exactly as before.
//
// The result is cached at module scope so the sub-nav and each admin page
// share one probe per full page load instead of re-asking per mount.
//
// Four states, and the difference between the last two is load-bearing:
//
//   "unknown"     → the probe has not settled yet (render nothing / a spinner)
//   "unavailable" → it settled without an answer (network failure, 401, 5xx —
//                   twice, see below). The admin pages used to fold this into
//                   "unknown" and render null for good: a blank page with no
//                   hint and no way back short of a reload. They now render a
//                   "couldn't check your permissions — Retry" notice instead
//                   (AdminGateFallback), wired to retryAdminProbe().
//
// A probe that fails is re-run once automatically before it is called
// unavailable: the common cause is a tab restored mid-reconnect or a server
// that is still coming up, and one more try a moment later usually answers.

export type AdminState = "unknown" | "admin" | "member" | "unavailable";

// A settled ANSWER (admin/member) — never "unavailable", which is retried.
let cached: AdminState | null = null;
let inflight: Promise<AdminState> | null = null;
// Every mounted hook instance, so a retry (or the automatic re-probe) reaches
// all of them — the sub-nav and the page — not just the one that asked.
const listeners = new Set<(state: AdminState) => void>();

const RETRY_DELAY_MS = 1500;
let retryDelayMs = RETRY_DELAY_MS;

async function probeOnce(): Promise<AdminState> {
  try {
    const res = await fetch("/api/admin/settings", { cache: "no-store" });
    if (res.ok || res.status === 501) return "admin";
    if (res.status === 403) return "member";
    // 401 and anything unexpected: no answer — do not flash the Admin
    // section in or out on a transient failure.
    return "unknown";
  } catch {
    return "unknown";
  }
}

function broadcast(state: AdminState) {
  for (const listener of listeners) listener(state);
}

// runProbe is the single shared probe: concurrent callers join the in-flight
// promise, and its result reaches every listener.
function runProbe(): Promise<AdminState> {
  inflight ??= (async () => {
    let result = await probeOnce();
    if (result === "unknown") {
      await new Promise((resolve) => setTimeout(resolve, retryDelayMs));
      result = await probeOnce();
    }
    const settled: AdminState = result === "unknown" ? "unavailable" : result;
    if (settled !== "unavailable") cached = settled;
    inflight = null;
    broadcast(settled);
    return settled;
  })();
  return inflight;
}

// retryAdminProbe re-runs the probe on demand (the fallback's Retry button).
// A cached answer is final for this page load and is returned as-is.
export function retryAdminProbe(): Promise<AdminState> {
  if (cached) return Promise.resolve(cached);
  broadcast("unknown");
  return runProbe();
}

export function useIsAdmin(): AdminState {
  const [state, setState] = useState<AdminState>(cached ?? "unknown");

  useEffect(() => {
    if (cached) return;
    // Subscribe first so a probe that settles between the render and this
    // effect still lands; then start (or join) the probe.
    listeners.add(setState);
    void runProbe();
    return () => {
      listeners.delete(setState);
    };
  }, []);

  return state;
}

// resetAdminProbeForTests clears the module cache between unit tests, and lets
// a test shorten the automatic re-probe delay so it does not have to wait.
export function resetAdminProbeForTests(opts: { retryDelayMs?: number } = {}) {
  cached = null;
  inflight = null;
  listeners.clear();
  retryDelayMs = opts.retryDelayMs ?? RETRY_DELAY_MS;
}
