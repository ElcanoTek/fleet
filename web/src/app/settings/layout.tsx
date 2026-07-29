"use client";

// Settings shell (fleet-unified settings pass): every /settings/* page renders
// inside the SAME NavRail + PageTopBar chrome as Chat and the Operations
// Center — settings is a third surface of the one app, not a separate island.
// The topbar reads "Settings"; the rail shows neither surface active and the
// account control below takes the `current` tint (the design's settings view).
//
// Inside the shell: the design's .settings-wrap — a sticky left sub-nav
// (SetNav) beside the .set-main content column. The content wrapper is keyed
// on the pathname so switching sections re-runs the set-fade entrance while
// the shell itself stays mounted (client-side navigation via the sub-nav's
// links).
//
// This layout replaced the standalone SettingsShell (own brand header +
// "Operations Center"/"Back to chat" pills); pages now own only their
// .set-section content.

import { useEffect, useState, type ReactNode } from "react";
import { usePathname } from "next/navigation";
import { useClientConfig } from "@/app/lib/useClientConfig";
import { NavRail, useRailCollapse } from "@/app/shared/ui/NavRail";
import { PageTopBar } from "@/app/shared/ui/PageTopBar";
import { SetNav } from "./SetNav";

// signOut posts the logout form (same semantics as the chat surface: the
// browser navigates to /api/auth/logout, clearing the session cookie).
function signOut() {
  const form = document.createElement("form");
  form.method = "post";
  form.action = "/api/auth/logout";
  document.body.appendChild(form);
  form.submit();
}

export default function SettingsLayout({ children }: { children: ReactNode }) {
  const pathname = usePathname();
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const collapse = useRailCollapse();
  const { branding } = useClientConfig();
  const [email, setEmail] = useState("");

  useEffect(() => {
    let stale = false;
    void (async () => {
      try {
        const res = await fetch("/api/session", { cache: "no-store" });
        if (res.status === 401) {
          window.location.href = "/login";
          return;
        }
        if (!res.ok) return;
        const data = (await res.json()) as { email?: string };
        if (!stale && data.email) setEmail(data.email);
      } catch {
        // Transient failure: the account button shows its loading state; every
        // page-level fetch still handles its own auth redirects.
      }
    })();
    return () => {
      stale = true;
    };
  }, []);

  return (
    <div className="grid h-[100dvh] grid-cols-[minmax(0,1fr)] overflow-hidden text-[var(--color-text-primary)] sm:grid-cols-[auto_minmax(0,1fr)]">
      <NavRail
        activeView="settings"
        brandName={branding.app_name}
        brandLogoSrc={branding.logo_url || undefined}
        sidebarOpen={sidebarOpen}
        setSidebarOpen={setSidebarOpen}
        collapse={collapse}
        account={{ email, onSignOut: signOut }}
      />
      {/* min-h-0 + overflow-hidden keep this grid item at the track height
          (its min-height:auto would otherwise grow it to the content and
          silently clip everything below the fold); the <main> then owns the
          scroll, so the topbar stays put — the design's .main/.view split. */}
      <div className="flex min-h-0 min-w-0 flex-col overflow-hidden">
        <PageTopBar title="Settings" onMenu={() => setSidebarOpen(true)} />
        {/* scrollbar-gutter:stable — sections differ in height (General/Admin
            fit the viewport; Connections/Skills overflow), so on classic-
            scrollbar platforms the scrollbar would otherwise appear and
            disappear across navigations, resizing the centered wrap and making
            the sub-nav + content jump. Reserving the gutter keeps every
            section at the same width. */}
        <main className="min-h-0 flex-1 overflow-y-auto [scrollbar-gutter:stable]">
          <div className="mx-auto flex w-full max-w-[74rem] items-start gap-9 px-6 pb-14 pt-7 max-[900px]:flex-col max-[900px]:gap-5">
            <SetNav />
            <div
              key={pathname}
              className="min-w-0 max-w-[46rem] flex-1 motion-safe:animate-set-fade"
            >
              {children}
            </div>
          </div>
        </main>
      </div>
    </div>
  );
}
