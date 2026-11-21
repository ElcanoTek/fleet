import { t as tr } from "@/app/lib/i18n";
// Guides shell (/help). Same chrome as Chat, Background tasks and
// Settings — one sidebar, one PageTopBar — because the guides are a surface of
// the app, not a link out to a docs site. That is the whole point of putting
// them here: a user who cannot remember what DEAD_LETTERED means is one click
// from the answer, signed in, in their own theme, without leaving the product.
//
// The sidebar is the one every page has (AppSidebar), with no top item lit
// (activeView="help"), the same way Settings does.

import { useState, type ReactNode } from "react";
import { useRailCollapse } from "@/app/shared/ui/NavRail";
import { AppSidebar } from "@/app/shared/sidebar/AppSidebar";
import { PageTopBar } from "@/app/shared/ui/PageTopBar";

export default function HelpLayout({ children }: { children: ReactNode }) {
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const collapse = useRailCollapse();

  return (
    <div className="grid h-[100dvh] grid-cols-[minmax(0,1fr)] overflow-hidden text-[var(--color-text-primary)] sm:grid-cols-[auto_minmax(0,1fr)]">
      <AppSidebar
        activeView="help"
        sidebarOpen={sidebarOpen}
        setSidebarOpen={setSidebarOpen}
        collapse={collapse}
      />
      {/* min-h-0 + overflow-hidden keep this grid item at the track height so
          <main> owns the scroll and the topbar stays put — the same main/view
          split every other surface uses. */}
      <div className="flex min-h-0 min-w-0 flex-col overflow-hidden">
        <PageTopBar title={tr("Guides")} onMenu={() => setSidebarOpen(true)} />
        <main className="min-h-0 flex-1 overflow-y-auto [scrollbar-gutter:stable]">
          <div className="mx-auto flex w-full max-w-[74rem] items-start gap-9 px-6 pb-16 pt-7 max-[900px]:flex-col max-[900px]:gap-5">
            {children}
          </div>
        </main>
      </div>
    </div>
  );
}
