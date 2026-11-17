import { t as tr } from "@/app/lib/i18n";
import type { ReactNode } from "react";
import { LaunchpadButton } from "@/app/launchpad/LaunchpadButton";
import { NavToChat, NavToOrchestrator } from "@/app/shared/ui/CrossViewNav";
import { Link } from "@/app/lib/Link";
import { navItemClass, navTileClass, RailLabel } from "@/app/shared/ui/railStyles";
import { writeDashboardView } from "@/app/chat/ui/dashboard/prefs";
import {
  builtinSubtitle,
  defaultIcon,
  itemName,
  kindName,
  pageHref,
  type SidebarItem,
} from "./layout";
import { HomeIcon, ItemIcon } from "./glyphs";
import { Icon } from "@/app/shared/ui/Icon";
import { openSidebarCustomizer, useSidebar } from "./store";
import { useFallbackTargets, usePatrolIds, useSidebarTargets } from "./targets";
import { useIsAdmin } from "@/app/settings/useIsAdmin";
import type { RailView } from "@/app/shared/ui/NavRail";

// RailNav — the rail's destinations, in the person's own order
// (docs/SIDEBAR.md). With the default layout it renders exactly the three
// items NavRail always had (Chat, Patrols, Background tasks, with their
// subtitles and test ids); a customized layout adds pinned shortcuts —
// chats, dashboards, patrols, projects, background tasks, pages — hides,
// renames and reorders them, and marks one as Home.

/** Where a shortcut leads when the surface cannot open it in place. */
export function shortcutHref(item: SidebarItem): string {
  const ref = encodeURIComponent(item.ref ?? "");
  switch (item.kind) {
    case "chat":
      return `/chat?c=${ref}`;
    case "project":
      return `/chat?project=${ref}`;
    case "task":
      return `/orchestrator?task=${ref}`;
    case "page":
      return pageHref(item.ref ?? "");
    default:
      return "/chat";
  }
}

/**
 * prepareShortcut runs before a shortcut is followed: a chat shortcut opens
 * the way it says (as a dashboard, or as a chat), so that choice is recorded
 * where the chat reads it. The surface then opens it in place when it can,
 * and otherwise the browser goes to its page.
 */
export function prepareShortcut(item: SidebarItem) {
  if (item.kind === "chat" && item.ref) writeDashboardView(item.ref, item.view === "dashboard");
}

export function RailNav({
  activeView,
  collapsed,
  opsCount,
  onNavigate,
}: {
  activeView: RailView;
  collapsed: boolean;
  opsCount?: number;
  onNavigate: () => void;
}) {
  const { layout, source } = useSidebar();
  const pageTargets = useSidebarTargets();
  const needsLists = !pageTargets && layout.items.some((i) => !i.hidden && (i.kind === "chat" || i.kind === "project"));
  const fallbackTargets = useFallbackTargets(needsLists);
  const isAdmin = useIsAdmin() === "admin";
  const targets = pageTargets ?? fallbackTargets;
  const hasChatShortcuts = layout.items.some((i) => i.kind === "chat" && !i.hidden && !i.icon);
  const patrolIds = usePatrolIds(hasChatShortcuts);
  const compact = Boolean(layout.compact);

  const isActive = (item: SidebarItem): boolean => {
    if (item.kind === "chat") return activeView === "chat" && targets?.activeConversationId === item.ref;
    if (item.kind === "project") return activeView === "chat" && targets?.activeProjectId === item.ref;
    if (item.kind === "page") {
      return (
        (item.ref === "analytics" && activeView === "analytics") ||
        (item.ref === "help" && activeView === "help") ||
        (item.ref === "settings" && activeView === "settings")
      );
    }
    return false;
  };

  const visible = layout.items.filter((item) => {
    if (item.hidden) return false;
    // The server refuses a task pin in a workspace default (only its owner
    // and admins can open a task); one arriving anyway is never shown.
    if (item.kind === "task") return source === "user";
    // Analytics is an admin page: a pin to it shows only to admins.
    if (item.kind === "page" && item.ref === "analytics") return isAdmin;
    if (item.kind !== "chat" && item.kind !== "project") return true;
    // A shortcut to something this account cannot open: the person's own is
    // kept (dimmed, so they can see and remove it); one that came from the
    // workspace default is not shown to them — and is not shown until it is
    // known to be openable, on every page, so a pin an admin made to their
    // own chat never leaks its name to everyone.
    if (source === "user") return true;
    return targets?.availability(item) === "yes";
  });
  const shortcutActive = visible.some((i) => i.kind !== "builtin" && isActive(i));

  const homeMark = (item: SidebarItem): ReactNode =>
    layout.home === item.id ? (
      <span
        className={["inline-flex shrink-0 text-[var(--color-accent)]", collapsed ? "sm:hidden" : ""].join(" ")}
        title={tr("Home — fleet opens here")}
      >
        <HomeIcon className="size-3" />
        <span className="sr-only">{tr("(Home)")}</span>
      </span>
    ) : null;

  const renderBuiltin = (item: SidebarItem) => {
    const id = item.id;
    const title = itemName(item);
    const subtitle = compact ? undefined : builtinSubtitle(id as "chat" | "patrols" | "tasks");
    const icon = item.icon || defaultIcon(item);
    if (id === "chat") {
      const active = activeView === "chat" && !shortcutActive;
      return (
        <NavToChat
          key={id}
          className={navItemClass(active, collapsed)}
          ariaCurrent={active ? "page" : undefined}
          dataTip={collapsed ? tr("{0} — talk it through", [title]) : undefined}
        >
          <span className={navTileClass(active)} aria-hidden="true">
            <ItemIcon icon={icon} className="size-[1rem]" />
          </span>
          <RailLabel title={title} subtitle={subtitle} collapsed={collapsed} trailing={homeMark(item)} />
        </NavToChat>
      );
    }
    if (id === "patrols") {
      return (
        <LaunchpadButton
          key={id}
          className={`${navItemClass(false, collapsed)} w-full cursor-pointer text-left`}
          tileClassName={navTileClass(false)}
          icon={item.icon ? <ItemIcon icon={item.icon} className="size-[1rem]" /> : undefined}
          label={<RailLabel title={title} subtitle={subtitle} collapsed={collapsed} />}
          dataTip={collapsed ? tr("{0} — chats that check in on a schedule", [title]) : undefined}
          collapsed={collapsed}
          onOpen={onNavigate}
        />
      );
    }
    const active = activeView === "orchestrator";
    return (
      <NavToOrchestrator
        key={id}
        className={navItemClass(active, collapsed)}
        ariaCurrent={active ? "page" : undefined}
        dataTip={
          collapsed
            ? typeof opsCount === "number" && opsCount > 0
              ? tr("{0} · {1} running", [title, opsCount])
              : tr("{0} · Runs without a chat", [title])
            : undefined
        }
      >
        <span className={navTileClass(active)} aria-hidden="true">
          <ItemIcon icon={icon} className="size-[1rem]" />
        </span>
        <RailLabel title={title} subtitle={subtitle} collapsed={collapsed} trailing={homeMark(item)} />
        {typeof opsCount === "number" && opsCount > 0 ? (
          <span
            className={[
              "font-[family-name:var(--font-code)] text-[0.7rem] text-[var(--color-text-muted)]",
              collapsed ? "sm:hidden" : "",
            ].join(" ")}
          >
            {opsCount}
          </span>
        ) : null}
      </NavToOrchestrator>
    );
  };

  const renderShortcut = (item: SidebarItem) => {
    const isPatrol = item.kind === "chat" && patrolIds.has(item.ref ?? "");
    const title = itemName(item, targets?.titleFor(item));
    const kind = kindName(item, isPatrol);
    const icon = item.icon || defaultIcon(item, isPatrol);
    const active = isActive(item);
    const unavailable = targets?.availability(item) === "no";
    const tip = unavailable ? tr("{0} — not available to you", [title]) : `${title} · ${kind}`;
    const body = (
      <>
        <span className={navTileClass(active)} aria-hidden="true">
          <ItemIcon icon={icon} className="size-[1rem]" />
        </span>
        <RailLabel
          title={title}
          subtitle={compact ? undefined : unavailable ? tr("Not available to you") : kind}
          collapsed={collapsed}
          trailing={homeMark(item)}
        />
      </>
    );
    const className = [navItemClass(active, collapsed), unavailable ? "opacity-55" : ""].join(" ");
    const common = {
      "data-testid": "rail-shortcut",
      "data-shortcut-kind": item.kind,
      "data-tip": collapsed ? tip : undefined,
      "aria-current": active ? ("page" as const) : undefined,
      title: !collapsed && unavailable ? tip : undefined,
      className,
    };
    if (item.kind === "page") {
      return (
        <Link key={item.id} href={shortcutHref(item)} {...common} onClick={onNavigate}>
          {body}
        </Link>
      );
    }
    return (
      <a
        key={item.id}
        href={shortcutHref(item)}
        {...common}
        onClick={(e) => {
          if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) {
            prepareShortcut(item);
            return;
          }
          prepareShortcut(item);
          if (targets?.open(item)) {
            e.preventDefault();
            onNavigate();
          }
        }}
      >
        {body}
      </a>
    );
  };

  return (
    <nav
      aria-label={tr("Switch surface")}
      data-testid="rail-nav"
      className={[
        "grid grid-cols-[minmax(0,1fr)] gap-0.5",
        // Many shortcuts scroll on their own rather than squeezing the chat
        // list. Expanded only: a scroller would also clip the collapsed
        // strip's hover labels, which are drawn beside it.
        collapsed ? "sm:justify-items-center" : "max-h-[42vh] overflow-y-auto overflow-x-hidden",
      ].join(" ")}
    >
      {visible.map((item) => (item.kind === "builtin" ? renderBuiltin(item) : renderShortcut(item)))}
    </nav>
  );
}

/**
 * CustomizeRailButton — the quiet way into the Customize panel from the rail
 * itself: a small pill on the divider under the destinations that shows when
 * the rail is hovered or the pill has keyboard focus.
 */
export function CustomizeRailButton({ collapsed }: { collapsed: boolean }) {
  return (
    <div className={["relative my-1 h-px shrink-0 bg-[var(--color-border)]", collapsed ? "" : "group/divider"].join(" ")}>
      {collapsed ? null : (
        <button
          type="button"
          data-testid="customize-sidebar-pill"
          onClick={openSidebarCustomizer}
          className="absolute left-1/2 top-1/2 inline-flex -translate-x-1/2 -translate-y-1/2 items-center gap-1 rounded-full border border-[var(--color-border)] bg-[var(--color-surface-2)] px-2 py-0.5 text-[0.66rem] font-medium text-[var(--color-text-muted)] opacity-0 shadow-[var(--shadow-sm)] transition hover:border-[var(--color-accent)] hover:text-[var(--color-text-primary)] focus-visible:opacity-100 focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] group-hover/rail:opacity-100 any-pointer-coarse:hidden"
        >
          <Icon name="edit" className="size-3" />
          {tr("Customize")}
        </button>
      )}
    </div>
  );
}
