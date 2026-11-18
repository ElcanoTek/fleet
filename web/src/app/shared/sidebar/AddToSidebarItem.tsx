import { t as tr } from "@/app/lib/i18n";
import { MenuItem } from "@/app/shared/ui/Menu";
import { Icon } from "@/app/shared/ui/Icon";
import { readDashboardView } from "@/app/chat/ui/dashboard/prefs";
import { addShortcut, findShortcut, removeItem, type ShortcutTarget } from "./layout";
import { updateSidebarLayout, useSidebar } from "./store";
import { useCanCustomize } from "./useCanCustomize";

// AddToSidebarItem — "Add to sidebar" / "Remove from sidebar" in a chat's or
// a project's menu: the quick way to pin one thing without opening the
// Customize panel. A chat the person last read as a dashboard is added as a
// dashboard shortcut (it opens the way they use it).
export function AddToSidebarItem({ target, onDone }: { target: ShortcutTarget; onDone: () => void }) {
  const { layout } = useSidebar();
  const canCustomize = useCanCustomize();
  const existing = findShortcut(layout, target.kind, target.ref);
  const inRail = Boolean(existing && !existing.hidden);
  if (!canCustomize) return null;
  return (
    <MenuItem
      icon={<Icon name="panel-left" className="size-4" />}
      testId="add-to-sidebar"
      onClick={() => {
        onDone();
        updateSidebarLayout((l) => {
          const found = findShortcut(l, target.kind, target.ref);
          if (found && !found.hidden) return removeItem(l, found.id);
          const view = target.kind === "chat" && readDashboardView(target.ref) ? "dashboard" : target.view;
          return addShortcut(l, { ...target, view });
        });
      }}
    >
      {inRail ? tr("Remove from sidebar") : tr("Add to sidebar")}
    </MenuItem>
  );
}
