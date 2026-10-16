"use client";

import { useEffect } from "react";
import { claimChunkAutoReload, isChunkLoadError } from "@/app/shared/chunkReload";

// ErrorScreen is what app/error.tsx and app/global-error.tsx render when a page
// throws. Before it existed a crash showed Next's bare "Application error: a
// client-side exception has occurred" text with no control at all, which in a
// browser tab cost a reload, and in an iPhone Home Screen web app (no address
// bar, no reload button) cost force-quitting the app. So the one thing this
// screen must always offer is a reload.
//
// A failed chunk load (a tab left open across a deploy) reloads by itself, once;
// see chunkReload.ts. Everything else waits for the reader.
//
// Navigation away is a hard one (location.assign) on purpose: the client router
// is part of what may have just failed, and a fresh document is the recovery.
export function ErrorScreen({ error }: { error: Error & { digest?: string } }) {
  useEffect(() => {
    console.error(error);
    if (isChunkLoadError(error) && claimChunkAutoReload()) {
      window.location.reload();
    }
  }, [error]);

  return (
    <main
      className="flex min-h-screen w-full items-center justify-center bg-[var(--color-bg)] px-6"
      style={{
        // statusBarStyle is black-translucent, so a Home Screen web app draws
        // under the status bar and the home indicator.
        paddingTop: "max(2.5rem, env(safe-area-inset-top))",
        paddingBottom: "max(2.5rem, env(safe-area-inset-bottom))",
      }}
    >
      <div
        role="alert"
        className="w-full max-w-sm rounded-[var(--radius-xl)] border border-[var(--color-border)] bg-[var(--color-surface-1)] p-6 shadow-[var(--shadow-md)]"
      >
        <h1 className="text-[1.25rem] font-semibold text-[var(--color-text-primary)]">
          Something went wrong
        </h1>
        <p className="mt-2 text-[0.875rem] text-[var(--color-text-secondary)]">
          This page hit an error. Reloading usually fixes it, and also picks up
          the latest version of the app if one has been released.
        </p>
        {error.digest ? (
          <p className="mt-2 font-[var(--font-code-ui)] text-[0.75rem] text-[var(--color-text-muted)]">
            Reference: {error.digest}
          </p>
        ) : null}
        <button
          type="button"
          onClick={() => window.location.reload()}
          className="mt-6 block w-full rounded-[var(--radius-md)] bg-[var(--color-primary)] px-4 py-2.5 text-center text-sm font-medium text-[var(--color-on-primary)] transition hover:bg-[var(--color-primary-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
        >
          Reload
        </button>
        <button
          type="button"
          onClick={() => window.location.assign("/")}
          className="mt-3 block w-full rounded-[var(--radius-md)] border border-[var(--color-border)] px-4 py-2.5 text-center text-sm font-medium text-[var(--color-text-primary)] transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
        >
          Go to the home page
        </button>
      </div>
    </main>
  );
}
