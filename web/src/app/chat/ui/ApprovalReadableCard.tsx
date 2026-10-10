"use client";

// The readable approval card (docs/APPROVAL-CARD-DESCRIBERS.md). A bundle can
// declare a read-only "describer" tool for a critical tool; when that call is
// staged, the server calls it with the same arguments and stores its
// structured result with the approval as `card`. This module renders that
// card in plain words: what the action is, which records it touches (name,
// buyer-facing id, link), each change as before → after, and the flags a
// reviewer must not miss. The raw tool name and arguments stay one click away
// under "Details".
//
// The server validates the card strictly; parseApprovalCardData (history.ts)
// re-checks the shape on the client so a malformed payload falls back to the generic card instead of
// half-rendering. Every string renders as text — React escapes it — and a
// link renders only when it is an absolute https URL.

import type { ReactNode } from "react";
import { useMemo, useState } from "react";
import type { ApprovalCardData, ApprovalCardItem } from "./history";

// Items beyond this many collapse behind "Show N more".
export const CARD_ITEMS_VISIBLE = 10;
// More items than this get a search box.
export const CARD_SEARCH_THRESHOLD = 25;

function FlagBadge({ label, code }: { label: string; code: string }) {
  return (
    <span
      data-testid="approval-card-flag"
      data-flag-code={code}
      className="inline-flex items-center rounded-full border px-2 py-0.5 text-[0.68rem] font-medium"
      style={{
        borderColor: "var(--color-warning-border)",
        color: "var(--color-warning)",
      }}
    >
      {label}
    </span>
  );
}

function CardItem({ item }: { item: ApprovalCardItem }) {
  return (
    <li data-testid="approval-card-item" className="min-w-0 border-t border-[var(--color-border-subtle)] py-1.5 first:border-t-0">
      <div className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-0.5">
        <span className="break-words font-medium text-[var(--color-text-primary)]">{item.label}</span>
        {item.id ? (
          <span className="break-all font-mono text-[0.72rem] text-[var(--color-text-muted)]">{item.id}</span>
        ) : null}
        {item.link ? (
          <a
            href={item.link}
            target="_blank"
            rel="noopener noreferrer"
            className="text-[0.72rem] text-[var(--color-accent)] underline-offset-2 hover:underline"
          >
            Open ↗
          </a>
        ) : null}
      </div>
      {item.flags && item.flags.length > 0 ? (
        <div className="mt-1 flex flex-wrap gap-1">
          {item.flags.map((f, i) => (
            <FlagBadge key={`${f.code}-${i}`} code={f.code} label={f.label} />
          ))}
        </div>
      ) : null}
      {item.changes && item.changes.length > 0 ? (
        <div className="mt-1 grid gap-0.5 text-[0.78rem] text-[var(--color-text-secondary)]">
          {item.changes.map((c, i) => (
            <div key={i} data-testid="approval-card-change" className="min-w-0 break-words">
              <span className="text-[var(--color-text-muted)]">{c.label}: </span>
              {c.before !== undefined ? (
                <>
                  <span className="line-through decoration-[var(--color-text-muted)]">{c.before || "(empty)"}</span>
                  <span aria-label="changes to" className="mx-1 text-[var(--color-text-muted)]">
                    →
                  </span>
                </>
              ) : null}
              <span className="font-medium text-[var(--color-text-primary)]">{c.after || "(empty)"}</span>
            </div>
          ))}
        </div>
      ) : null}
      {item.settings && item.settings.length > 0 ? (
        <div className="mt-1 grid gap-0.5 text-[0.78rem] text-[var(--color-text-secondary)]">
          {item.settings.map((s, i) => (
            <div key={i} data-testid="approval-card-setting" className="min-w-0 break-words">
              <span className="text-[var(--color-text-muted)]">{s.label}: </span>
              <span>{s.value}</span>
            </div>
          ))}
        </div>
      ) : null}
    </li>
  );
}

function matches(item: ApprovalCardItem, needle: string): boolean {
  const n = needle.toLowerCase();
  return (
    item.label.toLowerCase().includes(n) ||
    (item.id ?? "").toLowerCase().includes(n) ||
    (item.flags ?? []).some((f) => f.label.toLowerCase().includes(n))
  );
}

// ApprovalCardItems renders the record list: the first CARD_ITEMS_VISIBLE,
// then "Show N more"; above CARD_SEARCH_THRESHOLD items, a search box narrows
// the list by name, id or flag.
export function ApprovalCardItems({ items }: { items: ApprovalCardItem[] }) {
  const [expanded, setExpanded] = useState(false);
  const [query, setQuery] = useState("");
  const searchable = items.length > CARD_SEARCH_THRESHOLD;
  const filtered = useMemo(
    () => (searchable && query.trim() ? items.filter((it) => matches(it, query.trim())) : items),
    [items, query, searchable],
  );
  const shown = expanded ? filtered : filtered.slice(0, CARD_ITEMS_VISIBLE);
  const hidden = filtered.length - shown.length;
  if (items.length === 0) return null;
  return (
    <div className="mt-1 min-w-0">
      {searchable ? (
        <input
          type="search"
          data-testid="approval-card-search"
          aria-label="Search the records on this card"
          placeholder={`Search ${items.length} records`}
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          className="mb-1.5 w-full rounded-md border border-[var(--color-border-strong)] bg-transparent px-2 py-1 text-[0.75rem] text-[var(--color-text-primary)]"
        />
      ) : null}
      <ul className="min-w-0">
        {shown.map((it, i) => (
          <CardItem key={`${it.id ?? ""}-${i}`} item={it} />
        ))}
      </ul>
      {searchable && query.trim() && filtered.length === 0 ? (
        <p className="text-[0.72rem] text-[var(--color-text-muted)]">No record matches “{query.trim()}”.</p>
      ) : null}
      {hidden > 0 ? (
        <button
          type="button"
          data-testid="approval-card-show-more"
          className="mt-1 text-[0.72rem] text-[var(--color-accent)] hover:underline"
          onClick={() => setExpanded(true)}
        >
          Show {hidden} more
        </button>
      ) : null}
    </div>
  );
}

// ApprovalReadableBody is the pending card's body: subtitle, records, footer.
export function ApprovalReadableBody({ card }: { card: ApprovalCardData }) {
  return (
    <div data-testid="approval-card-readable" className="min-w-0 text-[0.8125rem]">
      {card.subtitle ? (
        <p className="mb-1 break-words text-[0.78rem] text-[var(--color-text-secondary)]">{card.subtitle}</p>
      ) : null}
      <ApprovalCardItems items={card.items} />
      {card.footer ? (
        <p className="mt-1.5 break-words text-[0.72rem] text-[var(--color-text-muted)]">{card.footer}</p>
      ) : null}
    </div>
  );
}

// ApprovalCardDetails is the collapsed "Details" disclosure: the raw tool,
// its server and its arguments, exactly as the generic card shows them.
export function ApprovalCardDetails({ children }: { children: ReactNode }) {
  return (
    <details data-testid="approval-card-details" className="mt-2 min-w-0">
      <summary className="cursor-pointer text-[0.72rem] text-[var(--color-text-muted)] hover:text-[var(--color-text-secondary)]">
        Details
      </summary>
      <div className="mt-1 min-w-0">{children}</div>
    </details>
  );
}
