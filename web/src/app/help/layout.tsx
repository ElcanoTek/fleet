"use client";

// Guides shell (/help). Same chrome as Chat, the Operations Center and
// Settings — one NavRail, one PageTopBar — because the guides are a surface of
// the app, not a link out to a docs site. That is the whole point of putting
// them here: a user who cannot remember what DEAD_LETTERED means is one click
// from the answer, signed in, in their own theme, without leaving the product.
//
// The rail renders with neither Chat nor the Operations Center active
// (activeView="help"), the same way Settings does.

import { signOut } from "@/app/shared/signOut";
import { useEffect, useState, type ReactNode } from "react";
import { useClientConfig } from "@/app/lib/useClientConfig";
import { NavRail, useRailCollapse } from "@/app/shared/ui/NavRail";
import { PageTopBar } from "@/app/shared/ui/PageTopBar";

export default function HelpLayout({ children }: { children: ReactNode }) {
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
        // Transient failure: the account button shows its loading state. The
        // guides themselves are static text and render regardless.
      }
    })();
    return () => {
      stale = true;
    };
  }, []);

  return (
    <div className="grid h-[100dvh] grid-cols-[minmax(0,1fr)] overflow-hidden text-[var(--color-text-primary)] sm:grid-cols-[auto_minmax(0,1fr)]">
      <NavRail
        activeView="help"
        brandName={branding.app_name}
        brandLogoSrc={branding.logo_url || undefined}
        sidebarOpen={sidebarOpen}
        setSidebarOpen={setSidebarOpen}
        collapse={collapse}
        account={{ email, onSignOut: signOut }}
      />
      {/* min-h-0 + overflow-hidden keep this grid item at the track height so
          <main> owns the scroll and the topbar stays put — the same main/view
          split every other surface uses. */}
      <div className="flex min-h-0 min-w-0 flex-col overflow-hidden">
        <PageTopBar title="Guides" onMenu={() => setSidebarOpen(true)} />
        <main className="min-h-0 flex-1 overflow-y-auto [scrollbar-gutter:stable]">
          <div className="mx-auto flex w-full max-w-[74rem] items-start gap-9 px-6 pb-16 pt-7 max-[900px]:flex-col max-[900px]:gap-5">
            {children}
          </div>
        </main>
      </div>
    </div>
  );
}
