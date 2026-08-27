"use client";

// SetNav — the settings area's sticky left sub-nav (the design's .set-nav).
// General · Team · Connections · Skills · Shared files, plus an expandable Admin parent whose
// children (Overview · Server · Doctor · Users · Features · Providers · Notifications) indent
// behind a left border and fade in. Admin renders only for admins (resolved
// client-side by useIsAdmin; every admin API stays server-gated regardless).
// Clicking Admin lands on Overview; the parent reads expanded whenever the
// route is inside /settings/admin.
//
// ≤900px the nav leaves its sticky column and becomes a wrapping horizontal
// row above the content (the design's narrow-viewport rules); the active
// item's 2px left bar hides there.

import Link from "next/link";
import { usePathname } from "next/navigation";
import { Icon } from "@/app/shared/ui/Icon";
import { useIsAdmin } from "./useIsAdmin";

const SECTIONS = [
  { href: "/settings", label: "General" },
  { href: "/settings/team", label: "Team" },
  { href: "/settings/connections", label: "Connections" },
  { href: "/settings/skills", label: "Skills" },
  // Member-visible on purpose: the page itself adapts (read-only library for
  // members, manage controls for admins) — NOT an ADMIN_SUBS entry.
  { href: "/settings/shared-files", label: "Shared files" },
] as const;

export const ADMIN_SUBS = [
  { href: "/settings/admin", label: "Overview" },
  { href: "/settings/admin/server", label: "Server" },
  { href: "/settings/admin/doctor", label: "Doctor" },
  { href: "/settings/admin/users", label: "Users" },
  { href: "/settings/admin/features", label: "Features" },
  { href: "/settings/admin/providers", label: "Providers" },
  { href: "/settings/admin/notifications", label: "Notifications" },
] as const;

const ITEM_BASE =
  "relative flex w-full items-center rounded-[var(--radius-md)] text-left no-underline transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]";

const ACTIVE_BAR =
  "before:absolute before:left-0 before:top-1/2 before:h-[0.85rem] before:w-0.5 before:-translate-y-1/2 before:rounded-full before:bg-[var(--color-primary)] max-[900px]:before:hidden";

function itemClass(active: boolean, child = false): string {
  return [
    ITEM_BASE,
    child ? "px-[0.55rem] py-[0.34rem] text-[0.78rem]" : "px-[0.6rem] py-[0.42rem] text-[0.82rem]",
    active
      ? `bg-[color-mix(in_srgb,var(--color-primary)_18%,transparent)] font-semibold text-[var(--color-text-primary)] ${ACTIVE_BAR}`
      : "text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]",
  ].join(" ");
}

export function SetNav() {
  const pathname = usePathname();
  const adminState = useIsAdmin();
  const inAdmin = pathname === "/settings/admin" || pathname.startsWith("/settings/admin/");

  return (
    <nav
      aria-label="Settings sections"
      className="sticky top-7 flex w-[11rem] shrink-0 flex-col gap-[1.1rem] max-[900px]:static max-[900px]:w-full max-[900px]:flex-row max-[900px]:flex-wrap max-[900px]:items-center max-[900px]:justify-between max-[900px]:gap-3"
    >
      <div className="grid gap-[0.15rem] max-[900px]:flex max-[900px]:flex-wrap max-[900px]:gap-[0.2rem]">
        {SECTIONS.map((s) => {
          const active = pathname === s.href;
          return (
            <Link
              key={s.href}
              href={s.href}
              aria-current={active ? "page" : undefined}
              className={itemClass(active)}
            >
              {s.label}
            </Link>
          );
        })}
        {adminState === "admin" ? (
          <>
            <Link
              href="/settings/admin"
              aria-expanded={inAdmin}
              data-testid="setnav-admin"
              className={[
                itemClass(false),
                "justify-between",
                inAdmin ? "font-semibold text-[var(--color-text-primary)]" : "",
              ].join(" ")}
            >
              Admin
              <Icon
                name="chevron-right"
                className={[
                  "size-[0.8rem] shrink-0 text-[var(--color-text-muted)] transition",
                  inAdmin ? "rotate-90" : "",
                ].join(" ")}
              />
            </Link>
            {inAdmin ? (
              <div className="relative mb-[0.15rem] ml-[0.6rem] mt-[0.1rem] grid gap-[0.1rem] border-l border-[var(--color-border)] pl-[0.55rem] motion-safe:animate-set-fade max-[900px]:ml-0 max-[900px]:flex max-[900px]:flex-wrap max-[900px]:gap-[0.2rem] max-[900px]:border-l-0 max-[900px]:pl-0">
                {ADMIN_SUBS.map((s) => {
                  const active = pathname === s.href;
                  return (
                    <Link
                      key={s.href}
                      href={s.href}
                      aria-current={active ? "page" : undefined}
                      className={itemClass(active, true)}
                    >
                      {s.label}
                    </Link>
                  );
                })}
              </div>
            ) : null}
          </>
        ) : null}
      </div>
    </nav>
  );
}

export default SetNav;
