// SetNav — the settings area's sticky left sub-nav (the design's .set-nav),
// organized into 3 functional groups:
//   1. Workspace (General, Team)
//   2. Model Context & Tools (Connections, Skills, Personas, Memories, Prompts, Shared files)
//   3. Admin (Overview, Server, System checks, Users, Features, Providers — rendered only for admins)
//
// ≤900px the nav leaves its sticky column and stacks the groups above the
// content with each group's items wrapping horizontally.

import { Link } from "@/app/lib/Link";
import { usePathname } from "@/app/lib/navigation";
import { Icon } from "@/app/shared/ui/Icon";
import { useIsAdmin } from "./useIsAdmin";

const WORKSPACE_SECTIONS = [
  { href: "/settings", label: "General", icon: "settings" },
  { href: "/settings/team", label: "Team", icon: "persona" },
] as const;

const CONTEXT_SECTIONS = [
  { href: "/settings/connections", label: "Connections", icon: "plug" },
  { href: "/settings/skills", label: "Skills", icon: "sparkles" },
  { href: "/settings/personas", label: "Personas", icon: "layers" },
  { href: "/settings/memories", label: "Memories", icon: "brain" },
  // Shared between Chat and the Operations Center: /api/orchestrator/prompts
  // authorizes signed-in Chat members by their Fleet role even when their
  // Background tasks access (ops_scope) is "none".
  { href: "/settings/prompts", label: "Prompts", icon: "book" },
  // Member-visible on purpose: the page itself adapts (read-only library for
  // members, manage controls for admins) — NOT an ADMIN_SUBS entry.
  { href: "/settings/shared-files", label: "Shared files", icon: "folder" },
] as const;

export const ADMIN_SUBS = [
  { href: "/settings/admin", label: "Overview" },
  { href: "/settings/admin/server", label: "Server" },
  { href: "/settings/admin/doctor", label: "System checks" },
  { href: "/settings/admin/users", label: "Users" },
  { href: "/settings/admin/features", label: "Features" },
  { href: "/settings/admin/providers", label: "Providers" },
] as const;

const ITEM_BASE =
  "relative flex w-full items-center gap-2 rounded-[var(--radius-md)] text-left no-underline transition max-[900px]:w-auto focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]";

const ACTIVE_BAR =
  "before:absolute before:left-0 before:top-1/2 before:h-[0.85rem] before:w-0.5 before:-translate-y-1/2 before:rounded-full before:bg-[var(--color-primary)] max-[900px]:before:hidden";

const GROUP_HEADING =
  "px-[0.6rem] pb-[0.2rem] text-[0.65rem] font-bold uppercase tracking-[0.08em] text-[var(--color-text-muted)] max-[900px]:px-1 max-[900px]:pb-1";

function itemClass(active: boolean, compact = false): string {
  return [
    ITEM_BASE,
    compact ? "px-[0.6rem] py-[0.36rem] text-[0.79rem]" : "px-[0.6rem] py-[0.42rem] text-[0.82rem]",
    active
      ? `bg-[color-mix(in_srgb,var(--color-primary)_18%,transparent)] font-semibold text-[var(--color-text-primary)] ${ACTIVE_BAR}`
      : "text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]",
  ].join(" ");
}

export function SetNav() {
  const pathname = usePathname();
  const adminState = useIsAdmin();

  return (
    <nav
      aria-label="Settings sections"
      className="sticky top-7 flex w-[11.5rem] shrink-0 flex-col gap-4 max-[900px]:static max-[900px]:w-full max-[900px]:flex-col max-[900px]:items-stretch max-[900px]:gap-2.5"
    >
      <div
        data-testid="setnav-group-workspace"
        className="grid gap-[0.12rem]"
      >
        <div className={GROUP_HEADING}>Workspace</div>
        <div className="grid gap-[0.12rem] max-[900px]:flex max-[900px]:flex-wrap max-[900px]:gap-[0.25rem]">
          {WORKSPACE_SECTIONS.map((s) => {
            const active = pathname === s.href;
            return (
              <Link
                key={s.href}
                href={s.href}
                aria-current={active ? "page" : undefined}
                className={itemClass(active)}
              >
                <Icon
                  name={s.icon}
                  className="size-3.5 shrink-0 text-[var(--color-text-muted)]"
                />
                <span>{s.label}</span>
              </Link>
            );
          })}
        </div>
      </div>

      <div
        data-testid="setnav-group-context"
        className="grid gap-[0.12rem]"
      >
        <div className={GROUP_HEADING}>Model Context &amp; Tools</div>
        <div className="grid gap-[0.12rem] max-[900px]:flex max-[900px]:flex-wrap max-[900px]:gap-[0.25rem]">
          {CONTEXT_SECTIONS.map((s) => {
            const active = pathname === s.href;
            return (
              <Link
                key={s.href}
                href={s.href}
                aria-current={active ? "page" : undefined}
                className={itemClass(active)}
              >
                <Icon
                  name={s.icon}
                  className="size-3.5 shrink-0 text-[var(--color-text-muted)]"
                />
                <span>{s.label}</span>
              </Link>
            );
          })}
        </div>
      </div>

      {adminState === "admin" ? (
        <div
          data-testid="setnav-group-admin"
          className="grid gap-[0.12rem]"
        >
          <div className={GROUP_HEADING}>Admin</div>
          <div className="grid gap-[0.12rem] max-[900px]:flex max-[900px]:flex-wrap max-[900px]:gap-[0.25rem]">
            {ADMIN_SUBS.map((s) => {
              const active = pathname === s.href;
              return (
                <Link
                  key={s.href}
                  href={s.href}
                  data-testid={s.href === "/settings/admin" ? "setnav-admin" : undefined}
                  aria-current={active ? "page" : undefined}
                  className={itemClass(active, true)}
                >
                  <span>{s.label}</span>
                </Link>
              );
            })}
          </div>
        </div>
      ) : null}
    </nav>
  );
}

export default SetNav;
