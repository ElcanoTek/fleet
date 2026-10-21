"use client";

import { usePathname } from "next/navigation";
import { useEffect, useState } from "react";
import { startDeployDriftProbe, useUpdateAvailable } from "./deployDrift";
import { CloseButton } from "./ui/CloseButton";
import { Icon } from "./ui/Icon";

// Signed-out surfaces: /api/version answers 401 there (or the viewer is an
// anonymous share-link reader), and a fresh sign-in loads the current build
// anyway.
function isSignedOutPath(pathname: string): boolean {
  return pathname === "/login" || pathname === "/no-access" || pathname.startsWith("/shared/");
}

// UpdateNotice tells the reader, on whatever page they are on, that a newer
// version of the app has been deployed, and offers the reload. It sits in the
// root layout so the Operations Center, Settings and Guides notice a deploy as
// well as chat, and it floats over the page so it shows on a phone, where the
// chat sidebar that used to carry this notice is a closed drawer. It owns the
// one deploy probe per tab (deployDrift.ts).
//
// It never reloads by itself (see deployDrift.ts for the bug that taught us),
// and it can be dismissed: on a phone it sits over the page header.
export function UpdateNotice({ appName }: { appName: string }) {
  const pathname = usePathname() ?? "";
  const enabled = !isSignedOutPath(pathname);
  const updateAvailable = useUpdateAvailable();
  const [dismissed, setDismissed] = useState(false);

  useEffect(() => {
    if (!enabled) return;
    return startDeployDriftProbe();
  }, [enabled]);

  if (!enabled || !updateAvailable || dismissed) return null;

  return (
    <div
      role="status"
      data-testid="update-notice"
      // Above page chrome, below dialogs (DialogShell is z-50) and toasts.
      className="fixed left-1/2 z-[45] flex max-w-[calc(100vw-1rem)] -translate-x-1/2 items-center gap-2 rounded-[var(--radius-lg)] border border-[var(--color-border-strong)] bg-[var(--color-surface-2)] py-1 pl-3 pr-1 text-[0.8125rem] text-[var(--color-text-primary)] shadow-[var(--shadow-md)] motion-safe:animate-pop-up"
      style={{ top: "calc(env(safe-area-inset-top, 0px) + 0.5rem)" }}
    >
      <Icon name="refresh" className="size-3.5 shrink-0 text-[var(--color-accent)]" />
      <span className="min-w-0 truncate">{appName} was updated</span>
      <button
        type="button"
        onClick={() => window.location.reload()}
        className="shrink-0 rounded-[var(--radius-md)] bg-[var(--color-primary-fill)] px-2.5 py-1 font-medium text-[var(--color-on-primary-fill)] transition enabled:hover:bg-[var(--color-primary-fill-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
      >
        Reload
      </button>
      <CloseButton label="Dismiss update notice" onClick={() => setDismissed(true)} />
    </div>
  );
}
