import { t as tr } from "@/app/lib/i18n";
import type { ReactNode } from "react";
import { NavToOrchestrator } from "@/app/shared/ui/CrossViewNav";
import { Link } from "@/app/lib/Link";
import { navItemClass, railIconClass, RailLabel } from "@/app/shared/ui/railStyles";
import { writeDashboardView } from "@/app/chat/ui/dashboard/prefs";
import { useNowSeconds } from "@/app/chat/ui/chatRetention";
import {
  defaultIcon,
  itemName,
  kindName,
  pageHref,
  topItems,
  type SidebarItem,
} from "./layout";
import { HomeIcon, ItemIcon } from "./glyphs";
import { RowStatus } from "./RowStatus";
import { useSidebar } from "./store";
import { patrolsByConversation, useFallbackTargets, usePatrols, useSidebarTargets } from "./targets";
import { useIsAdmin } from "@/app/settings/useIsAdmin";
import type { RailView } from "@/app/shared/ui/NavRail";

// RailNav — the top of the sidebar, in the person's own order
// (docs/SIDEBAR.md): Patrols and Background tasks, then whatever they keep
// there — a dashboard, a chat, a project, a background task, a page. Each is
// one row: an icon, a name, and one quiet status when it matters (a patrol
// chat's next check, the start-page mark). Starting a chat is the pen button
// beside the search field, so there is no "Chat" row.

/** Where a shortcut leads when the surface cannot open it in place. */
export function shortcutHref(item: SidebarItem): string {
  const ref = encodeURIComponent(item.ref ?? "");
  switch (item.kind) {
    case "chat":
      // The view travels with the link, so opening it in a new tab or from a
      // copied URL opens it the way the shortcut says.
      return `/chat?c=${ref}&view=${item.view === "dashboard" ? "dashboard" : "chat"}`;
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
  const items = topItems(layout);
  const needsLists = !pageTargets && items.some((i) => i.kind === "chat" || i.kind === "project");
  const fallbackTargets = useFallbackTargets(needsLists);
  const isAdmin = useIsAdmin() === "admin";
  const targets = pageTargets ?? fallbackTargets;
  const hasChatShortcuts = items.some((i) => i.kind === "chat");
  const patrols = patrolsByConversation(usePatrols(hasChatShortcuts));
  // A patrol's next check is quoted to the minute or the hour.
  const now = useNowSeconds(60_000);

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

  const visible = items.filter((item) => {
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

  // The start page's mark takes the row's one status slot.
  const homeMark = (item: SidebarItem): ReactNode =>
    layout.home === item.id ? (
      <span
        className={["ml-auto inline-flex shrink-0 pl-1 text-[var(--color-link)]", collapsed ? "sm:hidden" : ""].join(" ")}
        title={tr("Start page — fleet opens here")}
      >
        <HomeIcon className="size-3.5" />
        <span className="sr-only">{tr("(Start page)")}</span>
      </span>
    ) : null;

  const icon = (item: SidebarItem, active: boolean, isPatrol = false) => (
    <span aria-hidden="true" className="flex shrink-0">
      <ItemIcon icon={item.icon || defaultIcon(item, isPatrol)} className={railIconClass(active)} />
    </span>
  );

  const renderBuiltin = (item: SidebarItem) => {
    const id = item.id;
    const title = itemName(item);
    if (id === "patrols") {
      const active = activeView === "patrols";
      return (
        <Link
          key={id}
          href="/patrols"
          data-testid="nav-to-patrols"
          className={navItemClass(active, collapsed)}
          aria-current={active ? "page" : undefined}
          data-tip={collapsed ? tr("{0} — chats that check in on a schedule", [title]) : undefined}
          onClick={onNavigate}
        >
          {icon(item, active)}
          <RailLabel title={title} collapsed={collapsed} />
        </Link>
      );
    }
    const active = activeView === "orchestrator";
    const running = typeof opsCount === "number" && opsCount > 0 ? opsCount : 0;
    return (
      <NavToOrchestrator
        key={id}
        className={navItemClass(active, collapsed)}
        ariaCurrent={active ? "page" : undefined}
        dataTip={
          collapsed
            ? running
              ? tr("{0} · {1} running", [title, running])
              : tr("{0} · Runs without a chat", [title])
            : undefined
        }
      >
        {icon(item, active)}
        <RailLabel title={title} collapsed={collapsed} />
        {homeMark(item) ??
          (running ? (
            <span
              title={tr("{0} running", [running])}
              className={[
                "ml-auto shrink-0 pl-1 text-[0.72rem] font-medium tabular-nums text-[var(--color-text-muted)]",
                collapsed ? "sm:hidden" : "",
              ].join(" ")}
            >
              {running}
            </span>
          ) : null)}
      </NavToOrchestrator>
    );
  };

  const renderShortcut = (item: SidebarItem) => {
    const patrol = item.kind === "chat" ? patrols.get(item.ref ?? "") : undefined;
    const title = itemName(item, targets?.titleFor(item));
    const kind = kindName(item, Boolean(patrol));
    const active = isActive(item);
    const unavailable = targets?.availability(item) === "no";
    const tip = unavailable ? tr("{0} — not available to you", [title]) : `${title} · ${kind}`;
    const body = (
      <>
        {icon(item, active, Boolean(patrol))}
        <RailLabel title={title} collapsed={collapsed} />
        {homeMark(item) ??
          (patrol ? (
            <span className={["ml-auto inline-flex shrink-0", collapsed ? "sm:hidden" : ""].join(" ")}>
              <RowStatus status={{ patrol: { status: patrol.status, nextRunAt: patrol.next_run_at } }} now={now} />
            </span>
          ) : null)}
      </>
    );
    const className = [navItemClass(active, collapsed), unavailable ? "opacity-55" : ""].join(" ");
    const common = {
      "data-testid": "rail-shortcut",
      "data-shortcut-kind": item.kind,
      "data-tip": collapsed ? tip : undefined,
      "aria-current": active ? ("page" as const) : undefined,
      title: !collapsed ? (unavailable ? tip : `${title} · ${kind}`) : undefined,
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

  if (visible.length === 0) return null;
  return (
    <nav
      aria-label={tr("Switch surface")}
      data-testid="rail-nav"
      className={["group/nav grid shrink-0 grid-cols-[minmax(0,1fr)] gap-px", collapsed ? "sm:justify-items-center sm:gap-1" : ""].join(" ")}
    >
      {visible.map((item) => (item.kind === "builtin" ? renderBuiltin(item) : renderShortcut(item)))}
    </nav>
  );
}
