"use client";

import Image from "next/image";
import Link from "next/link";
import { usePathname } from "next/navigation";
import type { ReactNode } from "react";

// SettingsShell — the one frame every /settings/* page renders inside. Before
// this, Connections and Skills each hand-rolled the same header while the
// Operations Center kept its settings in a modal; the three surfaces read as
// disconnected islands. The shell gives them a single identity: the brand row
// ("Settings"), an Operations Center cross-link + the Back-to-chat pill, and a
// section nav (General · Connections · Skills) with the active page marked.
//
// Section pages own their content only — the shell owns the scroll (h-dvh +
// overflow-y-auto: document-level scrolling is unreliable on iOS under the app
// shell's html/body rules, see the note that used to live on each page).

const SECTIONS = [
  { href: "/settings", label: "General" },
  { href: "/settings/connections", label: "Connections" },
  { href: "/settings/skills", label: "Skills" },
  // Admin self-gates server-side (allowlist) — the tab is visible to everyone
  // exactly like the account-menu item, and a non-admin gets the page's own
  // "not on the allowlist" notice rather than a hidden surface.
  { href: "/admin", label: "Admin" },
] as const;

export function SettingsShell({
  title,
  description,
  actions,
  wide = false,
  children,
}: {
  /** The active section's name — becomes the page's (visually hidden) h1. */
  title: string;
  /** One-paragraph explanation rendered under the section nav. */
  description?: ReactNode;
  /** Extra header controls (e.g. Admin's Refresh), left of the nav pills. */
  actions?: ReactNode;
  /** Admin's tables want the 4xl column; the other sections read best at 3xl. */
  wide?: boolean;
  children: ReactNode;
}) {
  const pathname = usePathname();
  return (
    <main className="h-dvh overflow-y-auto bg-[var(--gradient-bg-home-signature)] px-6 py-10 text-[var(--color-text-primary)]">
      <div className={`mx-auto w-full ${wide ? "max-w-4xl" : "max-w-3xl"}`}>
        <header className="mb-4 flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
          <Link href="/" className="flex shrink-0 items-center gap-2.5 no-underline">
            <Image
              src="/logos/elcano-mark-primary.svg"
              alt="Elcano"
              width={28}
              height={28}
              priority
            />
            <span className="font-heading text-[0.9375rem] font-semibold">Settings</span>
          </Link>
          {/* whitespace-nowrap on each pill so a narrow (mobile) header wraps
              the CLUSTER, never a single label's letters ("Operations Center"
              used to stack mid-word). */}
          <div className="flex flex-wrap items-center justify-end gap-2 text-[0.8125rem] text-[var(--color-text-secondary)]">
            {actions}
            <Link
              href="/orchestrator"
              className="whitespace-nowrap rounded-full border border-[var(--color-border-subtle)] px-3 py-1 transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
            >
              Operations Center
            </Link>
            <Link
              href="/"
              className="whitespace-nowrap rounded-full border border-[var(--color-border-strong)] px-3 py-1 transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
            >
              Back to chat
            </Link>
          </div>
        </header>

        <nav aria-label="Settings sections" className="mb-5 flex flex-wrap gap-1.5">
          {SECTIONS.map((s) => {
            const active = pathname === s.href;
            return (
              <Link
                key={s.href}
                href={s.href}
                aria-current={active ? "page" : undefined}
                className={`rounded-full border px-3 py-1 text-[0.8125rem] no-underline transition ${
                  active
                    ? "border-[var(--color-accent)] font-medium text-[var(--color-text-primary)]"
                    : "border-[var(--color-border-subtle)] text-[var(--color-text-secondary)] hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
                }`}
              >
                {s.label}
              </Link>
            );
          })}
        </nav>

        <h1 className="sr-only">{title}</h1>
        {description ? (
          <p className="mb-5 text-[0.875rem] text-[var(--color-text-secondary)]">{description}</p>
        ) : null}

        {children}
      </div>
    </main>
  );
}

export default SettingsShell;
