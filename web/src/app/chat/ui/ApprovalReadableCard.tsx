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
import type { RowSelection } from "./approvalNarrowing";
import { Icon } from "./Icon";
import { StatusChip } from "@/app/shared/ui/StatusChip";

// Items beyond this many collapse behind "Show N more".
export const CARD_ITEMS_VISIBLE = 10;
// More items than this get a search box.
export const CARD_SEARCH_THRESHOLD = 25;

export function FlagBadge({ label, code }: { label: string; code: string }) {
  return (
    <StatusChip tone="warning" testId="approval-card-flag" data-flag-code={code}>
      {label}
    </StatusChip>
  );
}

// LeftOutTag marks a row the person unticked before approving
// (docs/APPROVAL-NARROWING.md): the value was not sent.
export function LeftOutTag({ settled = false }: { settled?: boolean }) {
  return (
    <span
      data-testid="approval-card-left-out"
      className="shrink-0 rounded-full border border-[var(--color-border-strong)] px-1.5 text-[0.68rem] font-medium text-[var(--color-text-secondary)]"
    >
      {settled ? "Left out · unchecked before approval" : "Left out"}
    </span>
  );
}

function CardItem({ item, leftOut = false }: { item: ApprovalCardItem; leftOut?: boolean }) {
  return (
    <li
      data-testid="approval-card-item"
      data-left-out={leftOut || undefined}
      className="min-w-0 border-t border-[var(--color-border-subtle)] py-1.5 first:border-t-0"
    >
      <div className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-0.5">
        <span className={`break-words font-medium ${leftOut ? "text-[var(--color-text-secondary)]" : "text-[var(--color-text-primary)]"}`}>{item.label}</span>
        {leftOut ? <LeftOutTag settled /> : null}
        {item.id ? (
          <span className="break-all font-mono text-[0.72rem] text-[var(--color-card-text-muted)]">{item.id}</span>
        ) : null}
        {item.link ? (
          <a
            href={item.link}
            target="_blank"
            rel="noopener noreferrer"
            className="text-[0.72rem] text-[var(--color-card-link)] underline-offset-2 hover:underline"
          >
            {item.linkLabel ? `${item.linkLabel} ↗` : "Open ↗"}
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
              <span className="text-[var(--color-card-text-muted)]">{c.label}: </span>
              {c.before !== undefined ? (
                <>
                  <span className="line-through decoration-[var(--color-card-text-muted)]">{c.before || "(empty)"}</span>
                  <span aria-label="changes to" className="mx-1 text-[var(--color-card-text-muted)]">
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
              <span className="text-[var(--color-card-text-muted)]">{s.label}: </span>
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
export function ApprovalCardItems({ items, leftOut }: { items: ApprovalCardItem[]; leftOut?: ReadonlySet<string> }) {
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
          <CardItem key={`${it.id ?? ""}-${i}`} item={it} leftOut={it.key !== undefined && leftOut?.has(it.key) === true} />
        ))}
      </ul>
      {searchable && query.trim() && filtered.length === 0 ? (
        <p className="text-[0.72rem] text-[var(--color-card-text-muted)]">No record matches “{query.trim()}”.</p>
      ) : null}
      {hidden > 0 ? (
        <button
          type="button"
          data-testid="approval-card-show-more"
          className="mt-1 text-[0.72rem] text-[var(--color-card-link)] hover:underline"
          onClick={() => setExpanded(true)}
        >
          Show {hidden} more
        </button>
      ) : null}
    </div>
  );
}

// ApprovalReadableBody is the pending card's body: subtitle, records, footer.
export function ApprovalReadableBody({
  card,
  leftOut,
  selection,
}: {
  card: ApprovalCardData;
  // Rows the person left out of a narrowed approval (settled cards).
  leftOut?: ReadonlySet<string>;
  // A narrowable pending card's ticks (docs/APPROVAL-NARROWING.md): its rows
  // render with checkboxes.
  selection?: RowSelection;
}) {
  return (
    <div data-testid="approval-card-readable" className="min-w-0 text-[0.8125rem]">
      {card.subtitle ? (
        <p className="mb-1 break-words text-[0.78rem] text-[var(--color-text-secondary)]">{card.subtitle}</p>
      ) : null}
      {selection ? (
        <ReadablePendingRecords items={card.items} selection={selection} />
      ) : (
        <ApprovalCardItems items={card.items} leftOut={leftOut} />
      )}
      {card.footer ? (
        <p className="mt-1.5 break-words text-[0.72rem] text-[var(--color-card-text-muted)]">{card.footer}</p>
      ) : null}
    </div>
  );
}

// ApprovalCardDetails is the collapsed "Details" disclosure: the raw tool,
// its server and its arguments, exactly as the generic card shows them.
export function ApprovalCardDetails({ children }: { children: ReactNode }) {
  return (
    <details data-testid="approval-card-details" className="mt-2 min-w-0">
      <summary className="cursor-pointer text-[0.72rem] text-[var(--color-card-text-muted)] hover:text-[var(--color-text-secondary)]">
        Details
      </summary>
      <div className="mt-1 min-w-0">{children}</div>
    </details>
  );
}

// ── The pending card in the designer's layout (APPROVAL-CARD-DESCRIBERS.md) ──
//
// Only a card with a describer's readable card renders this way: an
// "APPROVAL NEEDED" eyebrow with the countdown on the right, the plain-words
// title and subtitle, the record in an inner box (name, monospace buyer-facing
// id, "Open in <system>"), each change as label … before → after with the
// values right-aligned, and Approve / Cancel with Details on the same row.

// Rows (changes + settings) a single record shows before "Show all N".
export const CARD_ROWS_VISIBLE = 8;

// RecordLink is a record's external link: its label when the bundle names one
// ("Open in Index Exchange"), else "Open".
export function RecordLink({ link, linkLabel }: { link: string; linkLabel?: string }) {
  return (
    <a
      href={link}
      target="_blank"
      rel="noopener noreferrer"
      data-testid="approval-card-link"
      className="inline-flex items-center gap-1 text-[0.75rem] font-medium text-[var(--color-card-link)] underline-offset-2 hover:underline"
    >
      {linkLabel || "Open"}
      <Icon name="external-link" className="size-3" />
    </a>
  );
}

// ChangeValue renders before → after with the new value emphasised.
export function ChangeValue({ before, after }: { before?: string; after: string }) {
  return (
    <span className="min-w-0 break-words text-right">
      {before !== undefined ? (
        <>
          <span className="text-[var(--color-card-text-muted)]">{before || "(empty)"}</span>
          <span aria-label="changes to" className="mx-1.5 text-[var(--color-card-text-muted)]">
            →
          </span>
        </>
      ) : null}
      <span className="font-semibold text-[var(--color-text-primary)]">{after || "(empty)"}</span>
    </span>
  );
}

// CardRows lists a record's changes and settings as label … value rows,
// collapsing after CARD_ROWS_VISIBLE behind "Show all N".
export function CardRows({ changes, settings }: Pick<ApprovalCardItem, "changes" | "settings">) {
  const [expanded, setExpanded] = useState(false);
  const rows = [
    ...(changes ?? []).map((c, i) => ({ kind: "change" as const, key: `c${i}`, label: c.label, change: c })),
    ...(settings ?? []).map((st, i) => ({ kind: "setting" as const, key: `s${i}`, label: st.label, value: st.value })),
  ];
  if (rows.length === 0) return null;
  const shown = expanded ? rows : rows.slice(0, CARD_ROWS_VISIBLE);
  const hidden = rows.length - shown.length;
  return (
    <div className="min-w-0">
      <ul className="min-w-0">
        {shown.map((r) => (
          <li
            key={r.key}
            data-testid={r.kind === "change" ? "approval-card-change" : "approval-card-setting"}
            className="flex min-w-0 items-baseline justify-between gap-3 border-t border-[var(--color-border-subtle)] py-1.5 text-[0.8rem]"
          >
            <span className="shrink-0 text-[var(--color-text-secondary)]">{r.label}</span>
            {r.kind === "change" ? (
              <ChangeValue before={r.change.before} after={r.change.after} />
            ) : (
              <span className="min-w-0 break-words text-right text-[var(--color-text-primary)]">{r.value}</span>
            )}
          </li>
        ))}
      </ul>
      {hidden > 0 ? (
        <button
          type="button"
          data-testid="approval-card-rows-more"
          className="mt-0.5 text-[0.75rem] font-medium text-[var(--color-card-link)] hover:underline"
          onClick={() => setExpanded(true)}
        >
          Show all {rows.length}
        </button>
      ) : null}
    </div>
  );
}

// RecordBox is one record in the inner box: its name, its monospace
// buyer-facing id and its link, and any flags. `children` adds lines (a
// result card's status and ids).
export function RecordBox({
  label,
  id,
  link,
  linkLabel,
  flags,
  children,
  testId = "approval-card-item",
}: {
  label: string;
  id?: string;
  link?: string;
  linkLabel?: string;
  flags?: Array<{ code: string; label: string }>;
  children?: ReactNode;
  testId?: string;
}) {
  return (
    <div
      data-testid={testId}
      className="min-w-0 rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-surface-1)] px-3 py-2"
    >
      <div className="min-w-0 break-words [overflow-wrap:anywhere] text-[0.85rem] font-medium leading-[1.45] text-[var(--color-text-primary)]">{label}</div>
      {id || link ? (
        <div className="mt-0.5 flex min-w-0 flex-wrap items-baseline gap-x-3 gap-y-0.5">
          {id ? <span className="break-all font-mono text-[0.75rem] text-[var(--color-card-text-muted)]">{id}</span> : null}
          {link ? <RecordLink link={link} linkLabel={linkLabel} /> : null}
        </div>
      ) : null}
      {flags && flags.length > 0 ? (
        <div className="mt-1 flex flex-wrap gap-1">
          {flags.map((f, i) => (
            <FlagBadge key={`${f.code}-${i}`} code={f.code} label={f.label} />
          ))}
        </div>
      ) : null}
      {children}
    </div>
  );
}

// PlanRow is one record of a several-record card: name, id and link on the
// left, its single change's before → after on the right, flags below.
function PlanRow({ item, selection }: { item: ApprovalCardItem; selection?: RowSelection }) {
  const single = item.changes && item.changes.length === 1 && !(item.settings && item.settings.length) ? item.changes[0] : null;
  // A row of a narrowable card (docs/APPROVAL-NARROWING.md): a checkbox, and
  // once unticked the row greys out and reads "Left out".
  const selectable = selection !== undefined && item.key !== undefined && selection.selectable(item.key);
  const out = selectable && selection.isLeftOut(item.key);
  return (
    <li
      data-testid="approval-card-item"
      data-left-out={out || undefined}
      className="min-w-0 border-t border-[var(--color-border-subtle)] py-2 first:border-t-0"
    >
      <div className="flex min-w-0 items-start justify-between gap-3">
        {selectable && item.key !== undefined ? (
          <input
            type="checkbox"
            data-testid="approval-card-row-check"
            checked={!out}
            disabled={selection.disabled}
            onChange={() => selection.toggle(item.key as string)}
            aria-label={`Include ${item.label}${item.id ? ` (${item.id})` : ""}`}
            className="mt-1 size-3.5 shrink-0 accent-[var(--color-primary)]"
          />
        ) : null}
        <div className="min-w-0 flex-1">
          <div
            className={`break-words [overflow-wrap:anywhere] text-[0.82rem] font-medium ${out ? "text-[var(--color-text-secondary)]" : "text-[var(--color-text-primary)]"}`}
          >
            {item.label}
          </div>
          {item.id || item.link ? (
            <div className="mt-0.5 flex min-w-0 flex-wrap items-baseline gap-x-3">
              {item.id ? <span className="break-all font-mono text-[0.72rem] text-[var(--color-card-text-muted)]">{item.id}</span> : null}
              {item.link ? <RecordLink link={item.link} linkLabel={item.linkLabel} /> : null}
            </div>
          ) : null}
        </div>
        {out ? (
          <LeftOutTag />
        ) : single ? (
          <span data-testid="approval-card-change" className="shrink-0 text-[0.8rem]">
            <span className="sr-only">{single.label}: </span>
            <ChangeValue before={single.before} after={single.after} />
          </span>
        ) : null}
      </div>
      {item.flags && item.flags.length > 0 ? (
        <div className="mt-1 flex flex-wrap gap-1">
          {item.flags.map((f, i) => (
            <FlagBadge key={`${f.code}-${i}`} code={f.code} label={f.label} />
          ))}
        </div>
      ) : null}
      {single || out ? null : <CardRows changes={item.changes} settings={item.settings} />}
    </li>
  );
}

// ReadablePendingRecords is the pending card's record area: one record in
// its inner box with its rows below, or a list of records (collapsing after
// CARD_ITEMS_VISIBLE, searchable above CARD_SEARCH_THRESHOLD).
export function ReadablePendingRecords({ items, selection }: { items: ApprovalCardItem[]; selection?: RowSelection }) {
  const [expanded, setExpanded] = useState(false);
  const [query, setQuery] = useState("");
  const searchable = items.length > CARD_SEARCH_THRESHOLD;
  const filtered = useMemo(
    () => (searchable && query.trim() ? items.filter((it) => matches(it, query.trim())) : items),
    [items, query, searchable],
  );
  if (items.length === 0) return null;
  // One record reads in its inner box, unless it is a row the person may
  // untick: then it keeps the row layout, checkbox included.
  if (items.length === 1 && !(selection && selection.selectable(items[0].key))) {
    const it = items[0];
    return (
      <div className="mt-3 grid min-w-0 gap-2">
        <RecordBox label={it.label} id={it.id} link={it.link} linkLabel={it.linkLabel} flags={it.flags} />
        <CardRows changes={it.changes} settings={it.settings} />
      </div>
    );
  }
  const shown = expanded ? filtered : filtered.slice(0, CARD_ITEMS_VISIBLE);
  const hidden = filtered.length - shown.length;
  return (
    <div className="mt-3 min-w-0">
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
      <ul className="min-w-0 rounded-[var(--radius-md)] border border-[var(--color-border-subtle)] bg-[var(--color-surface-1)] px-3">
        {shown.map((it, i) => (
          <PlanRow key={`${it.key ?? it.id ?? ""}-${i}`} item={it} selection={selection} />
        ))}
      </ul>
      {searchable && query.trim() && filtered.length === 0 ? (
        <p className="mt-1 text-[0.72rem] text-[var(--color-card-text-muted)]">No record matches “{query.trim()}”.</p>
      ) : null}
      {hidden > 0 ? (
        <button
          type="button"
          data-testid="approval-card-show-more"
          className="mt-1 text-[0.75rem] font-medium text-[var(--color-card-link)] hover:underline"
          onClick={() => setExpanded(true)}
        >
          Show {hidden} more
        </button>
      ) : null}
    </div>
  );
}

// DetailsToggle is the "Details ⌄" control that sits on the action row; the
// panel it opens renders below the row (DetailsPanel), collapsed by default.
export function DetailsToggle({ open, onToggle, controls }: { open: boolean; onToggle: () => void; controls: string }) {
  return (
    <button
      type="button"
      data-testid="approval-card-details-toggle"
      aria-expanded={open}
      aria-controls={controls}
      onClick={onToggle}
      className="ml-auto inline-flex items-center gap-1 text-[0.75rem] text-[var(--color-card-text-muted)] hover:text-[var(--color-text-secondary)]"
    >
      Details
      <Icon name="chevron-down" className={`size-3 transition-transform ${open ? "rotate-180" : ""}`} />
    </button>
  );
}

export function DetailsPanel({ id, open, children }: { id: string; open: boolean; children: ReactNode }) {
  return (
    <div id={id} data-testid="approval-card-details" hidden={!open} className="mt-2 min-w-0 border-t border-[var(--color-border-subtle)] pt-2">
      {children}
    </div>
  );
}

// EyebrowCountdown is the pending card's top-right countdown: minutes while
// there is time, m:ss in the last two minutes, and the timed-out notice.
export function EyebrowCountdown({ remaining, expired }: { remaining: number | null; expired: boolean }) {
  if (remaining === null) return null;
  const text = expired
    ? "Timed out — the action was not taken."
    : remaining >= 120
      ? `Auto-declines in ${Math.ceil(remaining / 60)} min`
      : `Auto-declines in ${Math.floor(remaining / 60)}:${(remaining % 60).toString().padStart(2, "0")}`;
  return (
    <span
      data-testid="approval-countdown"
      className="inline-flex items-center gap-1 text-[0.72rem]"
      style={{ color: expired ? "var(--color-danger-text)" : "var(--color-card-text-muted)" }}
    >
      <Icon name="clock" className="size-3" />
      <span className={remaining < 120 && !expired ? "font-mono" : undefined}>{text}</span>
    </span>
  );
}
