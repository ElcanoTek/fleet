import { t as tr } from "@/app/lib/i18n";
// The Patrols page (/patrols, docs/SIDEBAR.md): where the sidebar's Patrols
// row leads. It used to open a menu; a person with more than a few patrols
// needs a page — every patrol they set up and the ones shared with them, each
// with its schedule, its last runs and its controls, and a way to start one.
//
// Same chrome as every other page: the one sidebar (AppSidebar) and a
// PageTopBar, here with what the page is for and its one primary action.

import { useState } from "react";
import { AppSidebar } from "@/app/shared/sidebar/AppSidebar";
import { useCustomizeAccess } from "@/app/shared/sidebar/useCanCustomize";
import { useRailCollapse } from "@/app/shared/ui/NavRail";
import { PageTopBar } from "@/app/shared/ui/PageTopBar";
import { Icon } from "@/app/shared/ui/Icon";
import { PatrolsView } from "./PatrolsView";

export default function PatrolsPage() {
  const [sidebarOpen, setSidebarOpen] = useState(false);
  const [adding, setAdding] = useState(false);
  const collapse = useRailCollapse();
  // A read-only Viewer's changes are all refused by the server (the `mutate`
  // gate), so the page offers them none. Only a confirmed Viewer loses the
  // controls: a role still being read, or one that could not be, keeps them.
  const canWrite = useCustomizeAccess().account !== "viewer";

  return (
    <div className="grid h-[100dvh] grid-cols-[minmax(0,1fr)] overflow-hidden text-[var(--color-text-primary)] sm:grid-cols-[auto_minmax(0,1fr)]">
      <AppSidebar activeView="patrols" sidebarOpen={sidebarOpen} setSidebarOpen={setSidebarOpen} collapse={collapse} />
      {/* min-h-0 + overflow-hidden keep this grid item at the track height so
          <main> owns the scroll and the topbar stays put — the same main/view
          split every other surface uses. */}
      <div className="flex min-h-0 min-w-0 flex-col overflow-hidden">
        <PageTopBar
          title={tr("Patrols")}
          subtitle={tr("Chats that check in on a schedule, with full history, workspace files and enabled connectors.")}
          icon="sparkles"
          onMenu={() => setSidebarOpen(true)}
          actions={!canWrite ? null : (
            <button
              type="button"
              data-testid="patrols-new"
              aria-expanded={adding}
              className="inline-flex h-9 items-center gap-1.5 rounded-[var(--radius-md)] bg-[var(--color-primary-fill)] px-3.5 text-[0.8125rem] font-semibold text-[var(--color-on-primary-fill)] transition hover:bg-[var(--color-primary-fill-hover)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] sm:h-8"
              onClick={() => setAdding(true)}
            >
              <Icon name="plus" className="size-4" />
              {tr("New patrol")}
            </button>
          )}
        />
        <main className="min-h-0 flex-1 overflow-y-auto [scrollbar-gutter:stable]">
          <PatrolsView adding={adding} setAdding={setAdding} canWrite={canWrite} />
        </main>
      </div>
    </div>
  );
}
