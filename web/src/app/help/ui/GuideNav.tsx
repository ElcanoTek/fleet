"use client";

// GuideNav — the /help sub-nav, the same sticky left column Settings uses
// (SetNav): the guide list, then the contents of whichever guide is open so a
// reader can jump between sections without scrolling back to the top. Below
// 900px it leaves the sticky column and becomes a wrapping row above the
// content, matching the settings surface's narrow-viewport behavior.

import Link from "next/link";
import { usePathname } from "next/navigation";
import type { Guide } from "../guideContent";
import type { Heading } from "../headings";

const ITEM_BASE =
  "relative flex w-full items-center rounded-[var(--radius-md)] text-left no-underline transition focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]";

const ACTIVE_BAR =
  "before:absolute before:left-0 before:top-1/2 before:h-[0.85rem] before:w-0.5 before:-translate-y-1/2 before:rounded-full before:bg-[var(--color-primary)] max-[900px]:before:hidden";

function itemClass(active: boolean): string {
  return [
    ITEM_BASE,
    "px-[0.6rem] py-[0.42rem] text-[0.82rem]",
    active
      ? `bg-[color-mix(in_srgb,var(--color-primary)_18%,transparent)] font-semibold text-[var(--color-text-primary)] ${ACTIVE_BAR}`
      : "text-[var(--color-text-secondary)] hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)]",
  ].join(" ");
}

export function GuideNav({
  guides,
  headings,
}: {
  guides: readonly Guide[];
  // Sections of the open guide; empty on the index, which has none.
  headings?: readonly Heading[];
}) {
  const pathname = usePathname();
  // Only top-level sections go in the contents list. The guides run to a dozen
  // sections each; adding every subsection would make the rail longer than the
  // viewport and harder to scan than the page itself.
  const sections = (headings ?? []).filter((h) => h.depth === 2);

  return (
    <nav
      aria-label="Guides"
      className="sticky top-7 flex w-[13rem] shrink-0 flex-col gap-[1.1rem] max-[900px]:static max-[900px]:w-full max-[900px]:flex-row max-[900px]:flex-wrap max-[900px]:items-start max-[900px]:gap-3"
    >
      <div className="grid gap-[0.15rem] max-[900px]:flex max-[900px]:flex-wrap max-[900px]:gap-[0.2rem]">
        <Link
          href="/help"
          aria-current={pathname === "/help" ? "page" : undefined}
          className={itemClass(pathname === "/help")}
        >
          Overview
        </Link>
        {guides.map((guide) => {
          const href = `/help/${guide.slug}`;
          const active = pathname === href;
          return (
            <Link
              key={guide.slug}
              href={href}
              aria-current={active ? "page" : undefined}
              className={itemClass(active)}
            >
              {guide.title}
            </Link>
          );
        })}
      </div>

      {sections.length > 0 ? (
        <div className="grid gap-[0.1rem] max-[900px]:hidden">
          <span className="px-[0.6rem] pb-1 text-[0.62rem] uppercase tracking-[0.1em] text-[var(--color-text-muted)]">
            On this page
          </span>
          {sections.map((heading) => (
            <a
              key={heading.id}
              href={`#${heading.id}`}
              className="rounded-[var(--radius-md)] px-[0.6rem] py-[0.28rem] text-[0.76rem] leading-[1.35] text-[var(--color-text-muted)] no-underline transition hover:bg-[var(--rail-hover)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)]"
            >
              {heading.text}
            </a>
          ))}
        </div>
      ) : null}
    </nav>
  );
}

export default GuideNav;
