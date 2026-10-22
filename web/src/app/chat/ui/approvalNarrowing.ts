// Approval narrowing (docs/APPROVAL-NARROWING.md): on a pending card whose
// tool declares a narrowable list argument, the readable card's rows that name
// a staged value get a checkbox, and the person approves only the ticked ones.
// These helpers hold the client half: which rows are selectable, what the
// approve request names, and the words the card uses. The server re-checks
// everything (a non-empty subset of the staged values) before it claims.

import { useCallback, useMemo, useState } from "react";
import type { Approval, ApprovalNarrowable, ApprovalNarrowed } from "./history";

/**
 * The keys of a card's rows a person may untick: rows whose key is one of the
 * staged values. Empty when the card is not narrowable, has no readable card,
 * or no row names a staged value — the card then approves its call whole.
 */
export function selectableKeys(approval: Pick<Approval, "narrowable" | "card">): ReadonlySet<string> {
  const n = approval.narrowable;
  const card = approval.card;
  if (!n || !card) return new Set();
  const staged = new Set(n.values);
  const keys = new Set<string>();
  for (const item of card.items) {
    if (item.key && staged.has(item.key)) keys.add(item.key);
  }
  return keys;
}

/** The staged values still ticked, in staged order: what an approval names. */
export function approvedValuesFor(n: ApprovalNarrowable, leftOut: ReadonlySet<string>): string[] {
  return n.values.filter((v) => !leftOut.has(v));
}

/** The approve button's words: "Approve" for the whole call, "Approve 3 of 5" when narrowed. */
export function narrowedApproveLabel(approved: number, total: number, whole = "Approve"): string {
  return approved >= total ? whole : `Approve ${approved} of ${total}`;
}

/** "3 of 5 selected": the live count shown above the rows. */
export function selectedCountText(approved: number, total: number): string {
  return `${approved} of ${total} selected`;
}

/** A settled card's line for a narrowed approval: "Approved 3 of 5 · 2 left out". */
export function narrowedSummary(n: ApprovalNarrowed): string {
  const left = n.staged.length - n.approved.length;
  return `Approved ${n.approved.length} of ${n.staged.length} · ${left} left out`;
}

/** The staged values a narrowed approval left out, as a set (for "Left out" rows). */
export function leftOutOf(n: ApprovalNarrowed | undefined): ReadonlySet<string> | undefined {
  if (!n) return undefined;
  const approved = new Set(n.approved);
  return new Set(n.staged.filter((v) => !approved.has(v)));
}

/** How a list of rows reads and changes the ticks (ReadablePendingRecords). */
export type RowSelection = {
  /** Whether the row with this key has a checkbox. */
  selectable: (key: string | undefined) => boolean;
  /** Whether the row with this key is unticked. */
  isLeftOut: (key: string | undefined) => boolean;
  toggle: (key: string) => void;
  disabled: boolean;
};

/**
 * One pending card's narrowing state. `active` is false for a card that
 * approves its call whole; then `approved` is null and nothing changes.
 */
export function useNarrowing(approval: Pick<Approval, "narrowable" | "card">, disabled: boolean) {
  const { narrowable, card } = approval;
  const selectable = useMemo(() => selectableKeys({ narrowable, card }), [narrowable, card]);
  const [leftOut, setLeftOut] = useState<ReadonlySet<string>>(() => new Set());
  const toggle = useCallback((key: string) => {
    setLeftOut((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  }, []);
  const active = narrowable !== undefined && selectable.size > 0;
  const approved = active && narrowable ? approvedValuesFor(narrowable, leftOut) : null;
  const total = narrowable?.values.length ?? 0;
  const selection: RowSelection | undefined = active
    ? {
        selectable: (key) => key !== undefined && selectable.has(key),
        isLeftOut: (key) => key !== undefined && leftOut.has(key),
        toggle,
        disabled,
      }
    : undefined;
  return { active, approved, total, leftOut, selection };
}
