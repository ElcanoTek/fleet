import { t as tr } from "@/app/lib/i18n";
import { MenuItem } from "@/app/shared/ui/Menu";
import { Icon } from "@/app/shared/ui/Icon";
import { readDashboardView } from "@/app/chat/ui/dashboard/prefs";
import { addShortcut, findShortcut, removeItem, type ShortcutTarget } from "./layout";
import { updateSidebarLayout, useSidebar } from "./store";

// AddToSidebarItem — "Add to sidebar" / "Remove from sidebar" in a chat's or
// a project's menu: the quick way to pin one thing without opening the
// Customize panel. A chat the person last read as a dashboard is added as a
// dashboard shortcut (it opens the way they use it).
export function AddToSidebarItem({ target, onDone }: { target: ShortcutTarget; onDone: () => void }) {
  const { layout } = useSidebar();
  const existing = findShortcut(layout, target.kind, target.ref);
  const inRail = Boolean(existing && !existing.hidden);
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

// TeamChatPinButton — the pin on a Team chats row (shown on hover or keyboard
// focus): when a team shares many chats, a person pins just the one they live
// in, and can then rename it in the Customize panel. Pressed again, it unpins.
export function TeamChatPinButton({ target, title }: { target: ShortcutTarget; title: string }) {
  const { layout } = useSidebar();
  const existing = findShortcut(layout, target.kind, target.ref);
  const inRail = Boolean(existing && !existing.hidden);
  const label = inRail ? tr("Remove {0} from the sidebar", [title]) : tr("Add {0} to the sidebar", [title]);
  return (
    <button
      type="button"
      aria-label={label}
      aria-pressed={inRail}
      title={label}
      data-testid="team-chat-pin"
      onClick={() =>
        updateSidebarLayout((l) => {
          const found = findShortcut(l, target.kind, target.ref);
          return found && !found.hidden ? removeItem(l, found.id) : addShortcut(l, target);
        })
      }
      className={[
        "absolute right-1 top-1/2 inline-flex size-7 -translate-y-1/2 items-center justify-center rounded-md bg-[var(--color-surface-2)] text-[var(--color-text-muted)] shadow-[var(--shadow-sm)] transition hover:text-[var(--color-text-primary)] focus-visible:opacity-100 focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:opacity-100",
        inRail ? "text-[var(--color-accent)]" : "",
        "opacity-0 group-hover/team:opacity-100",
      ].join(" ")}
    >
      <Icon name="panel-left" className="size-3.5" />
    </button>
  );
}
