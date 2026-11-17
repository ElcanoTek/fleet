import { t as tr } from "@/app/lib/i18n";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { useIsAdmin } from "@/app/settings/useIsAdmin";
import { Button } from "@/components/ui/button";
import { SidebarEditor } from "./SidebarEditor";
import { isBuiltinLayout } from "./layout";
import {
  resetSidebarLayout,
  retrySidebarSave,
  setSidebarCustomizerOpen,
  setSidebarLayout,
  useSidebar,
  useSidebarCustomizerOpen,
} from "./store";

// SidebarCustomizer — the Customize panel (docs/SIDEBAR.md). It slides in from
// the right WITHOUT covering the rail: it is a non-modal sheet, so the rail
// stays visible and usable beside it and every edit shows there as it is made
// (a live preview that is also the real thing). Edits save to the account a
// moment after the last one; "Reset" returns to the workspace default.

export function SidebarCustomizer() {
  const open = useSidebarCustomizerOpen();
  const { layout, source, status, saving, saveError, workspace } = useSidebar();
  const isAdmin = useIsAdmin() === "admin";
  const following = source !== "user";
  const resetTarget = workspace ? tr("your workspace's default") : tr("fleet's default");
  const atDefault = following || (workspace ? false : isBuiltinLayout(layout));

  return (
    <Sheet open={open} onOpenChange={setSidebarCustomizerOpen} modal={false}>
      {open ? (
        <SheetContent
          side="right"
          data-testid="sidebar-customizer"
          // Non-modal: a click on the rail (to try a shortcut) must not close
          // the panel. Escape and the close button still do.
          onInteractOutside={(e) => e.preventDefault()}
          className="w-full gap-0 p-0 sm:max-w-[27rem]"
        >
          <SheetHeader className="gap-1 border-b border-[var(--color-border)] px-5 pb-4 pt-5">
            <SheetTitle>{tr("Customize your sidebar")}</SheetTitle>
            <SheetDescription className="text-[0.8rem] leading-snug">
              {tr("Pin what you use, hide what you don't, and make fleet open where you work. Changes show in your sidebar as you make them.")}
            </SheetDescription>
            <p className="mt-1 flex flex-wrap items-center gap-2 text-[0.72rem] text-[var(--color-text-muted)]" data-testid="sidebar-save-state">
              {status === "unavailable" ? (
                <span className="text-[var(--color-status-warning-fg)]">
                  {tr("Your sidebar can't be saved right now — changes last until you reload.")}
                </span>
              ) : saveError ? (
                <>
                  <span role="alert" className="text-[var(--color-danger)]">
                    {tr("Couldn't save your sidebar.")}
                  </span>
                  <button
                    type="button"
                    onClick={retrySidebarSave}
                    className="rounded px-1 font-medium text-[var(--color-text-primary)] underline underline-offset-2"
                  >
                    {tr("Try again")}
                  </button>
                </>
              ) : saving ? (
                <span>{tr("Saving…")}</span>
              ) : following ? (
                <span>
                  {workspace
                    ? tr("You're using your workspace's default sidebar.")
                    : tr("You're using fleet's default sidebar.")}
                </span>
              ) : (
                <span className="inline-flex items-center gap-1">
                  <span className="size-1.5 rounded-full bg-[var(--color-status-success-fg)]" aria-hidden="true" />
                  {tr("Saved to your account — it follows you to any device.")}
                </span>
              )}
            </p>
          </SheetHeader>
          <div className="min-h-0 flex-1 overflow-y-auto px-5 py-4">
            <SidebarEditor layout={layout} onChange={setSidebarLayout} mode="user" isAdmin={isAdmin} />
          </div>
          <div className="flex items-center gap-2 border-t border-[var(--color-border)] px-5 py-3">
            <Button
              type="button"
              variant="ghost"
              size="sm"
              data-testid="sidebar-reset"
              disabled={atDefault}
              onClick={resetSidebarLayout}
              title={tr("Go back to {0}", [resetTarget])}
            >
              {tr("Reset to default")}
            </Button>
            <Button
              type="button"
              size="sm"
              className="ml-auto px-4"
              data-testid="sidebar-done"
              onClick={() => setSidebarCustomizerOpen(false)}
            >
              {tr("Done")}
            </Button>
          </div>
        </SheetContent>
      ) : null}
    </Sheet>
  );
}
