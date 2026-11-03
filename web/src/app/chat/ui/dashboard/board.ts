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
import { cardSpecOf, isRenderableCardCall, type GenUiState } from "../genui/transcript";

export type BoardPrefs = {
  /** Cards the user pinned to the board (forms included), as cardRef. */
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
  /** Keys this slot had before a refresh moved it (see deriveBoard). */
  aliases: string[];
  /** The card's own panel key, when the agent named one. */
  panelKey?: string;
  /** The newest card in this slot — what the panel draws. */
  cardId: string;
  call: ToolCall;
  spec: CardSpec;
  /** How many cards have fed this slot (1 = never updated). */
  version: number;
  /**
   * Which use of its card id the newest card is (providers can reuse ids):
   * namespaces its drafts and remounts the card when a reused id arrives.
   */
  occurrence: number;
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
  /**
   * Card id → every key (current and earlier) of the hidden slot it is in,
   * for cards whose panel the user took off the board: pinning the card
   * again clears all of them.
   */
  removedKeysOf: Map<string, string[]>;
};

/**
 * A card's identity for the user's per-card board choices: its tool-call id,
 * plus "#<occurrence>" for a later card that reused the id.
 */
export function cardRef(cardId: string, occurrence: number): string {
  // Ids are opaque: escape "%" and "#" so the "#<n>" suffix is unambiguous
  // ("call#1" the id and "call" the second time never share a reference).
  const id = cardId.replace(/%/g, "%25").replace(/#/g, "%23");
  return occurrence > 0 ? `${id}#${occurrence}` : id;
}

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
  // card reference (cardRef: id + occurrence) → slot key, for every card
  // placed so far. Lineage is followed by occurrence while walking the
  // transcript, so a later card reusing an id never breaks the chain of a
  // panel an earlier card with that id started.
  const slotOf = new Map<string, string>();
  const idOfRef = new Map<string, string>();
  // raw id → the reference of its latest occurrence so far: what a
  // `replaces` naming that id means at this point in the conversation.
  const liveRef = new Map<string, string>();
  // slot key → the reference of the card it currently shows.
  const slotRef = new Map<string, string>();
  const slots = new Map<string, BoardPanel>();
  const firstSeen = new Map<string, number>();
  // slot key → the keys it had before it moved (see below). A set, never
  // the slot's own key: alternating keys must not make it grow.
  const aliases = new Map<string, Set<string>>();
  let seq = 0;
  for (let idx = 0; idx < messages.length; idx++) {
    const m = messages[idx];
    if (m.role !== "assistant") continue;
    for (const tc of m.toolCalls ?? []) {
      if (!isRenderableCardCall(tc)) continue;
      const owns = genUi.owners.get(tc.id) === tc;
      const spec = owns ? genUi.cards.get(tc.id) : cardSpecOf(tc);
      if (!spec) continue;
      // A provider can reuse a tool-call id: the user's per-card choices
      // (pin, and the c: slot's removal, order and width) belong to one
      // occurrence of it, never to an unrelated later card.
      const ref = cardRef(tc.id, genUi.occurrences.get(tc) ?? 0);
      // The slot of the card this one refines — only while that card is the
      // one the slot shows. Refining a stale version takes nothing over,
      // matching deriveGenUiState's panel ownership.
      const replacedRef = spec.replaces ? liveRef.get(spec.replaces) : undefined;
      const replacedSlot = replacedRef !== undefined ? slotOf.get(replacedRef) : undefined;
      const lineage =
        replacedSlot !== undefined && slotRef.get(replacedSlot) === replacedRef ? replacedSlot : undefined;
      liveRef.set(tc.id, ref);
      idOfRef.set(ref, tc.id);
      const isPinned = pinned.has(ref);
      let key: string | undefined;
      if (spec.panel) key = `p:${spec.panel}`;
      else if (isPinned) key = `c:${ref}`;
      else if (lineage) key = lineage;
      else if (isDisplayOnly(spec)) key = `c:${ref}`;
      if (!key) continue;
      let prev = slots.get(key);
      if (lineage && lineage !== key) {
        // A refinement that also names a panel key (typically a pinned card
        // the agent refreshed and gave a key). The card it replaces is
        // superseded, so its slot never stays on the board beside this one:
        // - the key is new: the slot moves to it and keeps its place, version
        //   count and the user's layout choices;
        // - the key is taken: the old slot merges into that panel.
        const moved = slots.get(lineage);
        slots.delete(lineage);
        if (!prev) {
          prev = moved;
          firstSeen.set(key, firstSeen.get(lineage) ?? seq++);
        }
        // The old key's age goes with the slot: a later panel that reuses
        // the key is new, and is ordered as new.
        firstSeen.delete(lineage);
        slotRef.delete(lineage);
        const merged = new Set([...(aliases.get(key) ?? []), lineage, ...(aliases.get(lineage) ?? [])]);
        merged.delete(key);
        aliases.set(key, merged);
        aliases.delete(lineage);
        for (const [cid, k] of slotOf) if (k === lineage) slotOf.set(cid, key);
      }
      // A key that is live again (an unrelated card opened a new panel under
      // a key some slot moved away from) is that new panel's alone: it stops
      // being an alias, so the two never share removal, order or width.
      if (!prev) for (const set of aliases.values()) set.delete(key);
      slotOf.set(ref, key);
      slotRef.set(key, ref);
      if (!firstSeen.has(key)) firstSeen.set(key, seq++);
      slots.set(key, {
        key,
        panelKey: spec.panel ?? prev?.panelKey,
        cardId: tc.id,
        call: tc,
        spec,
        version: (prev?.version ?? 0) + 1,
        occurrence: genUi.occurrences.get(tc) ?? 0,
        aliases: [],
        messageIndex: idx,
        interactive: isInteractiveSpec(spec),
        wide: false,
        pinned: isPinned || (prev?.pinned ?? false),
      });
    }
  }
  // A slot still showing an older occurrence of a reused id shows a card
  // the transcript has replaced: it is not a panel.
  for (const [key, panel] of slots) if (genUi.owners.get(panel.cardId) !== panel.call) slots.delete(key);
  // A slot's earlier keys (after a move) still carry the user's choices.
  const keysOf = (key: string) => [key, ...(aliases.get(key) ?? new Set<string>())];
  const order = new Map(prefs.order.map((k, i) => [k, i]));
  const orderOf = (key: string) => keysOf(key).map((k) => order.get(k)).find((o) => o !== undefined);
  const wideOf = (key: string) => keysOf(key).map((k) => prefs.wide[k]).find((w) => w !== undefined);
  const panels = [...slots.values()]
    .filter((p) => !keysOf(p.key).some((k) => removed.has(k)))
    .map((p) => ({ ...p, aliases: [...(aliases.get(p.key) ?? [])], wide: wideOf(p.key) ?? prefersWide(p.spec) }))
    .sort((a, b) => {
      const oa = orderOf(a.key);
      const ob = orderOf(b.key);
      if (oa !== undefined && ob !== undefined) return oa - ob;
      if (oa !== undefined) return -1;
      if (ob !== undefined) return 1;
      return (firstSeen.get(a.key) ?? 0) - (firstSeen.get(b.key) ?? 0);
    });
  const shown = new Set(panels.map((p) => p.key));
  const cardSlots = new Map<string, string>();
  const removedKeysOf = new Map<string, string[]>();
  // Keyed by raw id, for the transcript's current card with each id only.
  for (const [ref, key] of slotOf) {
    const cardId = idOfRef.get(ref);
    if (cardId === undefined || liveRef.get(cardId) !== ref) continue;
    if (shown.has(key)) cardSlots.set(cardId, key);
    else if (slots.has(key)) removedKeysOf.set(cardId, keysOf(key));
  }
  return { panels, cardSlots, removedKeysOf };
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

/** What the server is told about the board on a dashboard-view turn. */
export type BoardManifestEntry = { title: string; card_id: string; panel?: string; pinned?: boolean };

/** Mirror internal/httpapi's maxDashboardBoardPanels / maxDashboardTitleRunes. */
export const MAX_MANIFEST_PANELS = 24;
export const MAX_MANIFEST_TITLE = 80;

/**
 * The board, in order, as POST /api/chat's `board` (docs/DASHBOARD-VIEW.md).
 * Bounded here to the limits the server applies anyway, so a large board can
 * never push a message past the request size cap.
 */
export function boardManifest(panels: BoardPanel[]): BoardManifestEntry[] {
  return panels.slice(0, MAX_MANIFEST_PANELS).map((p) => ({
    title: Array.from(p.spec.title).slice(0, MAX_MANIFEST_TITLE).join(""),
    card_id: p.cardId,
    ...(p.panelKey ? { panel: p.panelKey } : {}),
    ...(p.pinned ? { pinned: true } : {}),
  }));
}
