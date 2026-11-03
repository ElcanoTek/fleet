// Dashboard view board model (docs/DASHBOARD-VIEW.md). Pure: no React, so it
// unit-tests without jsdom.
//
// A board panel has no server row of its own, exactly like a show_ui card: it
// is DERIVED from the transcript. Each renderable card lands in a panel slot:
//
//   1. a card with a `panel` key lives in that panel ("p:<key>"); a later card
//      with the same key takes the panel over, so the agent refreshes a tile
//      by showing it again;
//   2. a card that `replaces` a card on the board inherits that card's slot,
//      so a refinement updates the tile it refines;
//   3. a display-only card (no inputs, no buttons), or one the user pinned,
//      gets a slot of its own ("c:<card id>").
//
// Anything else (a form without a panel key: a step in the conversation, not
// something to watch) stays inline in the chat rail. The user's own layout
// choices — removed panels, pins, order, width — are a per-browser overlay
// (BoardPrefs, stored by useBoardPrefs), never written to the transcript.

import type { Message, ToolCall } from "../history";
import { isInput, walkInputs, type CardSpec, type Component } from "../genui/model";
import { isRenderableCardCall, type GenUiState } from "../genui/transcript";

export type BoardPrefs = {
  /** Card ids the user pinned to the board (forms included). */
  pinned: string[];
  /** Panel keys the user took off the board; their cards return to the chat. */
  removed: string[];
  /** Panel keys in the user's chosen order; unlisted panels follow by age. */
  order: string[];
  /** Panel key → the user's width choice, overriding the content heuristic. */
  wide: Record<string, boolean>;
};

export const EMPTY_BOARD_PREFS: BoardPrefs = { pinned: [], removed: [], order: [], wide: {} };

export type BoardPanel = {
  /** Slot identity: "p:<panel key>" or "c:<first card id>". */
  key: string;
  /** The card's own panel key, when the agent named one. */
  panelKey?: string;
  /** The newest card in this slot — what the panel draws. */
  cardId: string;
  call: ToolCall;
  spec: CardSpec;
  /** How many cards have fed this slot (1 = never updated). */
  version: number;
  /** Index of the message holding the newest card (for "retired" and age). */
  messageIndex: number;
  /** Has inputs or buttons: the panel can send a message. */
  interactive: boolean;
  /** Drawn two columns wide. */
  wide: boolean;
  /** Placed by the user rather than by the card. */
  pinned: boolean;
};

export type Board = {
  panels: BoardPanel[];
  /**
   * Every card (current and earlier versions) whose slot is on the board,
   * mapped to that slot's key. The transcript draws these as a compact
   * pointer to the panel instead of a second copy of the card.
   */
  cardSlots: Map<string, string>;
};

/** A card with inputs or footer buttons can send something. */
export function isInteractiveSpec(spec: CardSpec): boolean {
  if ((spec.actions ?? []).length > 0) return true;
  let found = false;
  walkInputs(spec.components, () => {
    found = true;
  });
  return found;
}

const WIDE_TYPES = new Set(["chart", "table", "columns", "tabs", "diff"]);

function hasWideContent(list: Component[], depth = 0): boolean {
  if (depth > 6) return false;
  for (const c of list) {
    if (WIDE_TYPES.has(c.type)) {
      // A bar chart of a handful of labels reads fine in one column.
      if (c.type === "chart" && Array.isArray(c.labels) && c.labels.length <= 6 && c.kind !== "line") continue;
      return true;
    }
    const kids = Array.isArray(c.children) ? (c.children as Component[]) : [];
    if (kids.length > 0 && hasWideContent(kids, depth + 1)) return true;
  }
  return false;
}

/** Content heuristic for a panel's width: trends, tables and grids get room. */
export function prefersWide(spec: CardSpec): boolean {
  return hasWideContent(spec.components);
}

/** True when the card carries only display components (nothing to answer). */
export function isDisplayOnly(spec: CardSpec): boolean {
  return !isInteractiveSpec(spec) && !spec.components.some((c) => isInput(c));
}

/**
 * Derives the board from the transcript and the user's layout choices.
 * `genUi` is deriveGenUiState(messages) — passed in so the transcript and the
 * board share one derivation per render.
 */
export function deriveBoard(messages: Message[], genUi: GenUiState, prefs: BoardPrefs): Board {
  const removed = new Set(prefs.removed);
  const pinned = new Set(prefs.pinned);
  // card id → slot key, for every card placed so far (lineage lookups).
  const slotOf = new Map<string, string>();
  const slots = new Map<string, BoardPanel>();
  const firstSeen = new Map<string, number>();
  let seq = 0;
  for (let idx = 0; idx < messages.length; idx++) {
    const m = messages[idx];
    if (m.role !== "assistant") continue;
    for (const tc of m.toolCalls ?? []) {
      if (!isRenderableCardCall(tc)) continue;
      // A reused id belongs to its newest call; an older call with it is a
      // replaced card, never a panel.
      if (genUi.owners.get(tc.id) !== tc) continue;
      const spec = genUi.cards.get(tc.id);
      if (!spec) continue;
      let key: string | undefined;
      if (spec.panel) key = `p:${spec.panel}`;
      else if (spec.replaces && slotOf.has(spec.replaces)) key = slotOf.get(spec.replaces);
      else if (pinned.has(tc.id) || isDisplayOnly(spec)) key = `c:${tc.id}`;
      if (!key) continue;
      slotOf.set(tc.id, key);
      const prev = slots.get(key);
      if (!firstSeen.has(key)) firstSeen.set(key, seq++);
      slots.set(key, {
        key,
        panelKey: spec.panel ?? prev?.panelKey,
        cardId: tc.id,
        call: tc,
        spec,
        version: (prev?.version ?? 0) + 1,
        messageIndex: idx,
        interactive: isInteractiveSpec(spec),
        wide: false,
        pinned: pinned.has(tc.id) || (prev?.pinned ?? false),
      });
    }
  }
  const order = new Map(prefs.order.map((k, i) => [k, i]));
  const panels = [...slots.values()]
    .filter((p) => !removed.has(p.key))
    .map((p) => ({ ...p, wide: prefs.wide[p.key] ?? prefersWide(p.spec) }))
    .sort((a, b) => {
      const oa = order.get(a.key);
      const ob = order.get(b.key);
      if (oa !== undefined && ob !== undefined) return oa - ob;
      if (oa !== undefined) return -1;
      if (ob !== undefined) return 1;
      return (firstSeen.get(a.key) ?? 0) - (firstSeen.get(b.key) ?? 0);
    });
  const shown = new Set(panels.map((p) => p.key));
  const cardSlots = new Map<string, string>();
  for (const [cardId, key] of slotOf) if (shown.has(key)) cardSlots.set(cardId, key);
  return { panels, cardSlots };
}

/** The order after moving `key` by `delta` places among `keys` (clamped). */
export function moveKey(keys: string[], key: string, delta: number): string[] {
  const i = keys.indexOf(key);
  if (i < 0) return keys;
  const j = Math.max(0, Math.min(keys.length - 1, i + delta));
  if (i === j) return keys;
  const out = keys.slice();
  out.splice(i, 1);
  out.splice(j, 0, key);
  return out;
}

/** The order after dropping `key` onto `target`'s place. */
export function dropKey(keys: string[], key: string, target: string): string[] {
  if (key === target) return keys;
  const out = keys.filter((k) => k !== key);
  const j = out.indexOf(target);
  if (j < 0) return keys;
  const from = keys.indexOf(key);
  const to = keys.indexOf(target);
  // Dropped onto a later panel: land after it; onto an earlier one: before.
  out.splice(from < to ? j + 1 : j, 0, key);
  return out;
}

export type BoardStatus =
  | { kind: "standby" }
  | { kind: "working"; label: string | null }
  | { kind: "approval"; count: number };

/** What the HUD's status light shows: approvals first, then a live turn. */
export function boardStatus(messages: Message[], isStreaming: boolean): BoardStatus {
  let pending = 0;
  for (const m of messages) for (const a of m.approvals ?? []) if (a.status === "pending") pending++;
  if (pending > 0) return { kind: "approval", count: pending };
  if (!isStreaming) return { kind: "standby" };
  const last = messages[messages.length - 1];
  const running = last?.role === "assistant" ? (last.toolCalls ?? []).filter((t) => t.state === "pending").pop() : undefined;
  return { kind: "working", label: running ? running.name : null };
}
