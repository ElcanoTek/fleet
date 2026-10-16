"use client";

import { useSyncExternalStore } from "react";
import { currentBuildId } from "@/app/lib/buildId";

// Deploy drift: has the server moved to a newer build than the JavaScript this
// tab is running?
//
// Every response carries X-App-Version, and the bundle has its own build id
// baked in via NEXT_PUBLIC_BUILD_ID (next.config.ts). This module probes
// /api/version and remembers whether the two differ. It is ONE store per tab:
// UpdateNotice (mounted once from the root layout) starts the probe, and any
// component can read the result with useUpdateAvailable().
//
// It used to live inside the chat experience, which meant only /chat ever
// noticed a deploy, and the only sign of it was a button in the chat sidebar's
// footer: inside the drawer on a phone. That is the worst place for it on an
// iPhone Home Screen web app, which iOS freezes in the background and resumes
// days later still running the old bundle, with no address bar and no reload
// button. The Operations Center, Settings and Guides never checked at all.
//
// It never reloads the page by itself. An earlier version did, and it surfaced
// as "every click jumps me to the top": once a probe set the flag, the next
// stream-end handler force-reloaded mid-interaction. The reader decides when.
// (Client-side navigations are covered without us: Next compares the build id
// on every navigation payload and falls back to a full page load on a
// mismatch, so the stale cases left are pages that stay put, like a chat.)

// PROBE_INTERVAL_MS keeps a long-lived focused tab noticing deploys. A hidden
// tab skips the tick; it probes again the moment it becomes visible.
export const PROBE_INTERVAL_MS = 5 * 60 * 1000;

let updateAvailable = false;
const listeners = new Set<() => void>();
// A resumed tab fires visibilitychange and focus together; both share the one
// request already on the wire.
let inFlight: Promise<void> | null = null;

function setUpdateAvailable(next: boolean) {
  if (updateAvailable === next) return;
  updateAvailable = next;
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

function getSnapshot() {
  return updateAvailable;
}

// The server render never knows about a deploy the browser has not probed for.
function getServerSnapshot() {
  return false;
}

// useUpdateAvailable reports whether a newer build has been deployed since this
// tab loaded its JavaScript.
export function useUpdateAvailable(): boolean {
  return useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

// probeDeploy asks the server for its build id once. A failed probe (offline,
// signed out, a 5xx) changes nothing; the next event or tick tries again.
export function probeDeploy(): Promise<void> {
  const clientBuildId = currentBuildId();
  // No build id baked in means a dev server, where the id has no meaning.
  if (clientBuildId === "dev") return Promise.resolve();
  // Once drift is known it cannot un-happen for this bundle.
  if (updateAvailable) return Promise.resolve();
  if (!inFlight) {
    inFlight = fetchDrift(clientBuildId).finally(() => {
      inFlight = null;
    });
  }
  return inFlight;
}

async function fetchDrift(clientBuildId: string): Promise<void> {
  try {
    const res = await fetch("/api/version", { cache: "no-store" });
    if (!res.ok) return;
    const { buildId } = (await res.json()) as { buildId?: unknown };
    if (typeof buildId === "string" && buildId !== "" && buildId !== clientBuildId) {
      setUpdateAvailable(true);
    }
  } catch {
    // Network flake or a non-JSON body: try again on the next event or tick.
  }
}

// startDeployDriftProbe probes now, whenever the tab comes back into view or
// back online, and on an interval while it is visible. Returns the cleanup.
// Coming back into view is the event that matters on a phone: it is what a
// resumed Home Screen web app fires.
export function startDeployDriftProbe(): () => void {
  const probeIfVisible = () => {
    if (document.visibilityState === "hidden") return;
    void probeDeploy();
  };
  probeIfVisible();
  document.addEventListener("visibilitychange", probeIfVisible);
  window.addEventListener("focus", probeIfVisible);
  window.addEventListener("online", probeIfVisible);
  const interval = window.setInterval(probeIfVisible, PROBE_INTERVAL_MS);
  return () => {
    document.removeEventListener("visibilitychange", probeIfVisible);
    window.removeEventListener("focus", probeIfVisible);
    window.removeEventListener("online", probeIfVisible);
    window.clearInterval(interval);
  };
}

// __resetDeployDriftForTests clears the module state between test cases.
export function __resetDeployDriftForTests() {
  updateAvailable = false;
  inFlight = null;
  listeners.clear();
}
