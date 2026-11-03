// Per-browser dashboard view preferences (docs/DASHBOARD-VIEW.md): which
// chats open as a dashboard, and each board's layout overlay (removed panels,
// pins, order, width). Layout is a reading preference, like the transcript's
// "Show details" toggle, so it lives in localStorage rather than on the
// conversation: a teammate opening the same chat gets the panels the agent
// built, laid out by their own choices.
//
// Every access is wrapped: storage can be full, disabled or throw in a
// private window, and the view must still work (it just forgets on reload).

import { useCallback, useEffect, useState } from "react";
import { EMPTY_BOARD_PREFS, type BoardPrefs } from "./board";

const VIEW_KEY = "fleet.dashboard.view.v1";
const BOARD_KEY = "fleet.dashboard.board.v2";
/** Chats remembered as dashboards / boards remembered, newest first. */
export const MAX_REMEMBERED = 60;

function read<T>(key: string, fallback: T): T {
  try {
    const raw = window.localStorage.getItem(key);
    if (!raw) return fallback;
    const v = JSON.parse(raw) as unknown;
    return (v ?? fallback) as T;
  } catch {
    return fallback;
  }
}

function write(key: string, value: unknown): void {
  try {
    window.localStorage.setItem(key, JSON.stringify(value));
  } catch {
    // Full or blocked storage: the preference lasts for this page only.
  }
}

const strList = (v: unknown): string[] => (Array.isArray(v) ? v.filter((x): x is string => typeof x === "string") : []);

/** Whether this conversation was last left in dashboard view. */
export function readDashboardView(convId: string | null): boolean {
  if (!convId) return false;
  return strList(read<unknown>(VIEW_KEY, [])).includes(convId);
}

/** Records (or forgets) that this conversation opens in dashboard view. */
export function writeDashboardView(convId: string, on: boolean): void {
  const ids = strList(read<unknown>(VIEW_KEY, [])).filter((id) => id !== convId);
  if (on) ids.unshift(convId);
  write(VIEW_KEY, ids.slice(0, MAX_REMEMBERED));
}

// Stored as a list of entries, never as an object keyed by conversation id:
// an id is data, and indexing a plain object with it ("__proto__") would
// reach the prototype.
type StoredBoard = { id: string; prefs: unknown; at: number };

const UNSAFE_KEYS = new Set(["__proto__", "constructor", "prototype"]);

function storedBoards(): StoredBoard[] {
  const v = read<unknown>(BOARD_KEY, []);
  if (!Array.isArray(v)) return [];
  return v.filter(
    (e): e is StoredBoard =>
      !!e && typeof e === "object" && typeof (e as StoredBoard).id === "string" && typeof (e as StoredBoard).at === "number",
  );
}

function sanitize(v: unknown): BoardPrefs {
  if (!v || typeof v !== "object") return EMPTY_BOARD_PREFS;
  const o = v as Record<string, unknown>;
  const wide = new Map<string, boolean>();
  if (o.wide && typeof o.wide === "object" && !Array.isArray(o.wide)) {
    for (const [k, b] of Object.entries(o.wide as Record<string, unknown>)) {
      if (typeof b === "boolean" && !UNSAFE_KEYS.has(k)) wide.set(k, b);
    }
  }
  return { pinned: strList(o.pinned), removed: strList(o.removed), order: strList(o.order), wide: Object.fromEntries(wide) };
}

export function readBoardPrefs(convId: string | null): BoardPrefs {
  if (!convId) return EMPTY_BOARD_PREFS;
  return sanitize(storedBoards().find((e) => e.id === convId)?.prefs);
}

export function writeBoardPrefs(convId: string, prefs: BoardPrefs): void {
  // Newest first; keep the most recently edited boards only.
  const next = [{ id: convId, prefs, at: Date.now() }, ...storedBoards().filter((e) => e.id !== convId)]
    .sort((a, b) => b.at - a.at)
    .slice(0, MAX_REMEMBERED);
  write(BOARD_KEY, next);
}

/**
 * The board layout overlay for one conversation. `convId` is null for a chat
 * that has no server id yet (a draft): its choices are kept in memory and
 * written once the conversation has an id.
 */
export function useBoardPrefs(convId: string | null): [BoardPrefs, (update: (prev: BoardPrefs) => BoardPrefs) => void] {
  const [state, setState] = useState<{ convId: string | null; prefs: BoardPrefs }>(() => ({
    convId,
    prefs: readBoardPrefs(convId),
  }));
  // A different conversation: load its layout (a draft that just got its id
  // keeps what was chosen while it was a draft).
  let current = state;
  if (state.convId !== convId) {
    current = { convId, prefs: state.convId === null ? state.prefs : readBoardPrefs(convId) };
    setState(current);
  }
  useEffect(() => {
    if (current.convId && current.prefs !== EMPTY_BOARD_PREFS) writeBoardPrefs(current.convId, current.prefs);
  }, [current.convId, current.prefs]);
  const update = useCallback((fn: (prev: BoardPrefs) => BoardPrefs) => {
    setState((s) => ({ ...s, prefs: fn(s.prefs) }));
  }, []);
  return [current.prefs, update];
}
