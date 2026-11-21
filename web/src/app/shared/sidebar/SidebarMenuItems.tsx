import { t as tr } from "@/app/lib/i18n";
import { MenuItem } from "@/app/shared/ui/Menu";
import { Icon } from "@/app/shared/ui/Icon";
import { readDashboardView } from "@/app/chat/ui/dashboard/prefs";
import { HomeIcon } from "./glyphs";
import { isKeptAtTop, isStartPage, keepAtTop, removeFromTop, setHome, setStartPage, type ShortcutTarget } from "./layout";
import { updateSidebarLayout, useSidebar } from "./store";
import { useCanCustomize } from "./useCanCustomize";

// The two sidebar items in a chat's, a project's and a teammate's chat's ⋯
// menu (docs/SIDEBAR.md): the quick way to arrange the sidebar without
// opening Customize.
//
// "Move to the top" keeps the thing among the top items, above the lists,
// where it stays in view however long the lists get; it then leaves the list
// it was in (a thing is listed once). A chat the person last read as a
// dashboard is kept as a dashboard: it opens the way they use it.
//
// "Set as start page" makes it the page fleet opens to. It does not have to
// be at the top for that.
//
// Neither is offered to an account that cannot save its sidebar (a Viewer),
// or before the person's saved sidebar has loaded.

function withView(target: ShortcutTarget): ShortcutTarget {
  return target.kind === "chat" && readDashboardView(target.ref) ? { ...target, view: "dashboard" } : target;
}

export function KeepAtTopItem({ target, onDone }: { target: ShortcutTarget; onDone: () => void }) {
  const { layout } = useSidebar();
  const canCustomize = useCanCustomize();
  if (!canCustomize) return null;
  const atTop = isKeptAtTop(layout, target.kind, target.ref);
  return (
    <MenuItem
      icon={<Icon name="arrow-up" className={["size-4", atTop ? "rotate-180" : ""].join(" ")} />}
      testId="add-to-sidebar"
      onClick={() => {
        onDone();
        updateSidebarLayout((l) =>
          isKeptAtTop(l, target.kind, target.ref) ? removeFromTop(l, target.kind, target.ref) : keepAtTop(l, withView(target)),
        );
      }}
    >
      {atTop ? tr("Remove from the top") : tr("Move to the top")}
    </MenuItem>
  );
}

export function StartPageItem({ target, onDone }: { target: ShortcutTarget; onDone: () => void }) {
  const { layout } = useSidebar();
  const canCustomize = useCanCustomize();
  if (!canCustomize) return null;
  const isHome = isStartPage(layout, target.kind, target.ref);
  return (
    <MenuItem
      icon={<HomeIcon className="size-4" />}
      testId="set-start-page"
      onClick={() => {
        onDone();
        updateSidebarLayout((l) =>
          isStartPage(l, target.kind, target.ref) ? setHome(l, "") : setStartPage(l, withView(target)),
        );
      }}
    >
      {isHome ? tr("Remove as start page") : tr("Set as start page")}
    </MenuItem>
  );
}
