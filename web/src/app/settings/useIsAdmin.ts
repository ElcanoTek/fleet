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

export type AdminState = "unknown" | "admin" | "member";

let cached: AdminState | null = null;
let inflight: Promise<AdminState> | null = null;

async function probe(): Promise<AdminState> {
  try {
    const res = await fetch("/api/admin/settings", { cache: "no-store" });
    if (res.ok || res.status === 501) return "admin";
    if (res.status === 403) return "member";
    // 401 and anything unexpected: leave it unresolved rather than flashing
    // the Admin section in or out on a transient failure.
    return "unknown";
  } catch {
    return "unknown";
  }
}

export function useIsAdmin(): AdminState {
  const [state, setState] = useState<AdminState>(cached ?? "unknown");

  useEffect(() => {
    if (cached) return;
    let stale = false;
    inflight ??= probe().then((result) => {
      if (result !== "unknown") cached = result;
      inflight = null;
      return result;
    });
    void inflight.then((result) => {
      if (!stale) setState(result);
    });
    return () => {
      stale = true;
    };
  }, []);

  return state;
}

// resetAdminProbeForTests clears the module cache between unit tests.
export function resetAdminProbeForTests() {
  cached = null;
  inflight = null;
}
