import { t as tr } from "@/app/lib/i18n";
import type { ReactNode } from "react";
import { searchMatchRanges } from "../conversationOrganization";

// The parts of the sidebar's unified search results (docs/SIDEBAR-SEARCH.md):
// a group under its section's name, a result's tag, and the matched words
// marked in a title.

// ── Search results (the rail's unified search) ──────────────────────────────
// A small pill naming what a result is or where it lives ("Patrol",
// "Archived"): a result is shown out of its section, so it says so itself.
export function ResultTag({ children }: { children: ReactNode }) {
  return (
    <span className="shrink-0 rounded-full border border-[var(--color-border)] px-1.5 py-px text-[0.62rem] font-medium text-[var(--color-text-muted)]">
      {children}
    </span>
  );
}

// The matched words of a result's title.
export const MARK_CLASS =
  "rounded-[2px] bg-[color-mix(in_srgb,var(--color-accent)_25%,transparent)] text-[var(--color-text-primary)]";

export function markMatches(text: string, tokens: readonly string[]): ReactNode {
  const ranges = searchMatchRanges(text, tokens);
  if (ranges.length === 0) return text;
  const nodes: ReactNode[] = [];
  let last = 0;
  for (const [start, end] of ranges) {
    if (start > last) nodes.push(text.slice(last, start));
    nodes.push(
      <mark key={start} className={MARK_CLASS}>
        {text.slice(start, end)}
      </mark>,
    );
    last = end;
  }
  if (last < text.length) nodes.push(text.slice(last));
  return nodes;
}

// One group of results, under the name of the sidebar section its rows come
// from. When the person has hidden that section, the group says so — the
// reason these chats are nowhere else in the rail — and offers to bring the
// section back (to an account that may change its sidebar).
export function SearchGroup({
  testId,
  title,
  count,
  hidden,
  onShow,
  children,
}: {
  testId: string;
  title: string;
  count: number;
  hidden?: boolean;
  onShow?: () => void;
  children: ReactNode;
}) {
  return (
    <section data-testid={testId} aria-label={title} className="mb-2">
      <div className="flex items-center gap-1.5 px-2 pb-1 pt-1.5 text-[0.68rem] font-semibold uppercase tracking-[0.08em] text-[var(--color-text-muted)]">
        <span className="min-w-0 truncate">{title}</span>
        <span className="font-[family-name:var(--font-code)] font-normal tabular-nums">{count}</span>
      </div>
      {hidden ? (
        <p
          data-testid="search-hidden-note"
          className="mx-2 mb-1 flex flex-wrap items-center gap-x-2 gap-y-1 rounded-md bg-[var(--color-overlay-soft)] px-2 py-1 text-[0.72rem] text-[var(--color-text-secondary)]"
        >
          <span className="min-w-0 flex-1">{tr("Hidden in your sidebar. Search still finds what is in it.")}</span>
          {onShow ? (
            <button
              type="button"
              className="shrink-0 rounded px-1 font-semibold text-[var(--color-accent)] underline decoration-dotted underline-offset-2 transition hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] any-pointer-coarse:min-h-11"
              aria-label={tr("Show {0} in the sidebar", [title])}
              onClick={onShow}
            >
              {tr("Show it")}
            </button>
          ) : null}
        </p>
      ) : null}
      {children}
    </section>
  );
}
