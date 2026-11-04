"use client";

import { lazy, Suspense, useEffect, useState } from "react";
import { Icon } from "@/app/shared/ui/Icon";

// LaunchpadButton — the Patrol Launchpad's entry point in the shared top bar
// (PageTopBar), so it is one click away on Chat, the Operations Center,
// Settings and the Guides. Until someone has opened the Launchpad once, a
// small pulsing dot draws the eye to it; `?launchpad=1` on any of those pages
// opens it directly (the guides and docs link it that way).
//
// The wizard itself is lazy-loaded: it is not on the path to a first paint.

const PatrolLaunchpad = lazy(() => import("./PatrolLaunchpad"));

export const LAUNCHPAD_SEEN_ITEM = "fleet.launchpad.seen";
export const LAUNCHPAD_PARAM = "launchpad";

function readSeen(): boolean {
  try {
    return window.localStorage.getItem(LAUNCHPAD_SEEN_ITEM) === "1";
  } catch {
    return true; // No storage: never nag.
  }
}

function readDeepLink(): boolean {
  if (typeof window === "undefined") return false;
  return new URLSearchParams(window.location.search).get(LAUNCHPAD_PARAM) === "1";
}

export function LaunchpadButton() {
  const [open, setOpen] = useState(readDeepLink);
  const [seen, setSeen] = useState(() => readSeen() || readDeepLink());

  // Consume the one-shot deep link so a reload or a shared URL does not
  // reopen the wizard forever.
  useEffect(() => {
    if (!readDeepLink()) return;
    const url = new URL(window.location.href);
    url.searchParams.delete(LAUNCHPAD_PARAM);
    window.history.replaceState(window.history.state, "", url.pathname + url.search + url.hash);
  }, []);

  // Opened (by click or deep link) counts as seen: the dot is for discovery.
  useEffect(() => {
    if (!open) return;
    try {
      window.localStorage.setItem(LAUNCHPAD_SEEN_ITEM, "1");
    } catch {
      // Storage unavailable: the dot simply stays.
    }
  }, [open]);

  return (
    <>
      <button
        type="button"
        data-testid="launchpad-button"
        aria-label="Patrol Launchpad — set up a patrol"
        aria-haspopup="dialog"
        title="Patrol Launchpad — put an assistant on patrol over your tools"
        onClick={() => {
          setOpen(true);
          setSeen(true);
        }}
        className="relative inline-flex size-9 shrink-0 items-center justify-center gap-1.5 rounded-full border border-[color-mix(in_srgb,var(--color-primary)_42%,var(--color-border))] bg-[color-mix(in_srgb,var(--color-primary)_12%,var(--color-surface-1))] text-xs font-medium text-[var(--color-text-primary)] transition hover:border-[var(--color-primary)] hover:bg-[color-mix(in_srgb,var(--color-primary)_20%,var(--color-surface-1))] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] sm:h-8 sm:w-auto sm:px-3"
      >
        <Icon name="sparkles" className="size-3.5 shrink-0 text-[var(--color-primary)]" />
        <span className="hidden sm:inline">Launchpad</span>
        {!seen ? (
          <span aria-hidden="true" className="absolute -right-0.5 -top-0.5 flex size-2.5" data-testid="launchpad-new-dot">
            <span className="absolute inline-flex size-full rounded-full bg-[var(--color-primary-fill)] opacity-70 motion-safe:animate-ping" />
            <span className="relative inline-flex size-2.5 rounded-full bg-[var(--color-primary-fill)]" />
          </span>
        ) : null}
      </button>
      {open ? (
        <Suspense fallback={null}>
          <PatrolLaunchpad onClose={() => setOpen(false)} />
        </Suspense>
      ) : null}
    </>
  );
}
