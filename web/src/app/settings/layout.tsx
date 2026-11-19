import { t as tr } from "@/app/lib/i18n";
// Settings shell (fleet-unified settings pass): every /settings/* page renders
// inside the SAME sidebar + PageTopBar chrome as Chat and Background tasks —
// settings is a surface of the one app, not a separate island. The sidebar is
// the one every page has (AppSidebar: the person's pinned chats, projects and
// chats, so there is always a way back to a chat); the topbar reads
// "Settings", and the account control takes the `current` tint.
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

import { useState, type ReactNode } from "react";
import { usePathname } from "@/app/lib/navigation";
import { useRailCollapse } from "@/app/shared/ui/NavRail";
import { AppSidebar } from "@/app/shared/sidebar/AppSidebar";
import { PageTopBar } from "@/app/shared/ui/PageTopBar";
import { SetNav } from "./SetNav";

export default function SettingsLayout({ children }: { children: ReactNode }) {
  const pathname = usePathname();
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const collapse = useRailCollapse();

  return (
    <div className="grid h-[100dvh] grid-cols-[minmax(0,1fr)] overflow-hidden text-[var(--color-text-primary)] sm:grid-cols-[auto_minmax(0,1fr)]">
      <AppSidebar
        activeView="settings"
        sidebarOpen={sidebarOpen}
        setSidebarOpen={setSidebarOpen}
        collapse={collapse}
      />
      {/* min-h-0 + overflow-hidden keep this grid item at the track height
          (its min-height:auto would otherwise grow it to the content and
          silently clip everything below the fold); the <main> then owns the
          scroll, so the topbar stays put — the design's .main/.view split. */}
      <div className="flex min-h-0 min-w-0 flex-col overflow-hidden">
        <PageTopBar title={tr("Settings")} onMenu={() => setSidebarOpen(true)} />
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
              // w-full: below 900px this column stacks under the nav, where
              // without a set width it sized to its content, so a page with a
              // table or a file field (Users, Shared files) came out wider
              // than a phone. The help layout already does this.
              className="w-full min-w-0 max-w-[46rem] flex-1 motion-safe:animate-set-fade"
            >
              {children}
            </div>
          </div>
        </main>
      </div>
    </div>
  );
}
