import { createContext, useContext, useEffect, useMemo, useState, useSyncExternalStore } from "react";
import type { SidebarItem } from "./layout";

// What the surface knows about the things a sidebar shortcut points at. Chat
// provides it (it holds the conversations, team chats and projects): live
// names, which one is open, whether this account can still open a target, and
// an in-page opener so a click does not reload the app. Every other surface
// renders without it — shortcuts there show the name saved with them and
// navigate to Chat.

export type Availability = "yes" | "no" | "unknown";

/** One row of the sidebar's Pinned section, as Customize lists it. */
export type PinnedInfo = {
  /** Its name in the saved order: a chat's id, or "project:<id>". */
  id: string;
  kind: "chat" | "project";
  /** The chat's or the project's own id. */
  ref: string;
  title: string;
  /** Whether this account may unpin it (a project's pin is its owner's). */
  canUnpin: boolean;
};

/** The Pinned section, for the Customize panel and the gear's start-page list. */
export type SidebarPins = {
  entries: PinnedInfo[];
  /** Saves a new order; absent when this account cannot reorder. */
  reorder?: (ids: string[]) => void;
  unpin: (entry: PinnedInfo) => void;
  /** Pins a chat or a project by id; false when it is not one this page can pin. */
  pin: (kind: "chat" | "project", ref: string) => boolean;
  /** Whether `pin` would take this chat or project; the picker lists only those. Absent: every candidate is offered. */
  canPin?: (kind: "chat" | "project", ref: string) => boolean;
};

export type SidebarTargets = {
  activeConversationId: string | null;
  activeProjectId: string | null;
  /** The Pinned section; absent before the sidebar's lists exist. */
  pins?: SidebarPins;
  /** The target's current name, when this surface knows it. */
  titleFor: (item: SidebarItem) => string | undefined;
  availability: (item: SidebarItem) => Availability;
  /** Open the target in place. False: not handled here (the rail navigates). */
  open: (item: SidebarItem) => boolean;
};

export const SidebarTargetsContext = createContext<SidebarTargets | null>(null);

export function useSidebarTargets(): SidebarTargets | null {
  return useContext(SidebarTargetsContext);
}

// ── Patrols: which conversations are patrol chats, and when each checks next ──
// One read of GET /api/patrols per page, shared by every part of the sidebar
// that marks a patrol (a row's "next check" status, a shortcut's icon and
// kind) and by the Patrols page, and only when something asks. The Patrols
// page calls refreshPatrols() after it changes one, so the sidebar's marks
// follow without a reload.

/** One patrol, as GET /api/patrols lists it. */
export type PatrolBrief = {
  id: string;
  conversation_id: string;
  title: string;
  conversation_title: string;
  cron?: string;
  timezone?: string;
  status: "active" | "paused" | string;
  /** Unix seconds of the next run (an active patrol). */
  next_run_at?: number;
  last_run_at?: number;
  run_count?: number;
  /** The patrol's chat's approval mode. */
  approval_mode?: "manual" | "auto" | "autonomous";
  /** The patrol's owner, when it is a teammate's (shared with the caller). */
  owner_email?: string;
  /** False for a teammate's patrol the caller can only read. Absent (an older server) means the caller's own. */
  mine?: boolean;
  /** False when the patrol's chat is someone else's (the caller set the patrol up in a teammate's Multiplayer chat): its approval mode is the chat owner's to change. Absent (an older server) reads as the caller's chat. */
  owns_chat?: boolean;
  /** How a teammate's patrol's chat is shared with the caller. */
  multiplayer?: boolean;
  team_visible?: boolean;
  /** The caller's own active patrol is being skipped behind an approval card. */
  skipped?: { reason: string; count: number; slots?: number[] };
  /** What each run is asked to do: GET /api/patrols/{id} only, for its creator. */
  prompt?: string;
};

/** Whether the caller set this patrol up (and so may run, pause, edit and remove it). */
export function isOwnPatrol(p: PatrolBrief): boolean {
  return p.mine !== false;
}

const NO_PATROLS: readonly PatrolBrief[] = [];
let patrolList: readonly PatrolBrief[] = NO_PATROLS;
let patrolRead: Promise<readonly PatrolBrief[] | null> | null = null;
// Bumped by every refresh: a read that started before the latest refresh
// (a focus read overtaken by the read after a pause, edit or removal) must
// not publish its older list over the newer one.
let patrolGen = 0;
// Whether patrolRead has answered (a cached answer) or is still on its way.
let patrolReadDone = false;
const patrolListeners = new Set<() => void>();

/**
 * fetchPatrolList reads the index. A failure is null, never [] — "could not
 * load" must not read as "you have no patrols".
 */
export async function fetchPatrolList(): Promise<PatrolBrief[] | null> {
  try {
    const res = await fetch("/api/patrols", { cache: "no-store" });
    if (!res.ok) return null;
    const data = (await res.json()) as { patrols?: PatrolBrief[] };
    return Array.isArray(data.patrols) ? data.patrols : null;
  } catch {
    return null;
  }
}

function readPatrols(): Promise<readonly PatrolBrief[] | null> {
  if (!patrolRead) {
    const gen = patrolGen;
    patrolReadDone = false;
    const read: Promise<readonly PatrolBrief[] | null> = fetchPatrolList().then((list) => {
      if (patrolRead === read) patrolReadDone = true;
      if (list && gen === patrolGen) {
        patrolList = list;
        for (const l of patrolListeners) l();
      }
      // A failed read is not kept: the next part of the page that asks
      // (a mount, a page change) reads again instead of reusing the failure.
      if (!list && patrolRead === read) patrolRead = null;
      return list;
    });
    patrolRead = read;
  }
  return patrolRead;
}

/** Reads the patrols again and tells every part of the page that shows them. */
export function refreshPatrols(): Promise<readonly PatrolBrief[] | null> {
  patrolGen++;
  patrolRead = null;
  return readPatrols();
}

/**
 * forgetPatrol: a patrol the server has just removed comes off the list at
 * once, whether or not the read that follows succeeds, so it is never shown
 * as still there (and still to be acted on). A read already on its way
 * started before the removal and is not published.
 */
export function forgetPatrol(id: string): void {
  patrolGen++;
  patrolRead = null;
  patrolList = patrolList.filter((p) => p.id !== id);
  for (const l of patrolListeners) l();
}

/**
 * freshPatrols: a read that starts no earlier than now. A read still on its
 * way is shared (so a page that mounts while its sidebar is reading makes
 * one request, and the answer it judges by is the one it draws); an answer
 * already in hand, from earlier in the session, is read again.
 */
export function freshPatrols(): Promise<readonly PatrolBrief[] | null> {
  if (patrolRead && !patrolReadDone) return patrolRead;
  return refreshPatrols();
}

/**
 * patrolsChanged: a patrol was created, paused, resumed, edited or removed
 * somewhere that keeps its own copy (Chat's Patrol bar). The shared list is
 * read again now when something on the page shows it, and otherwise on the
 * next read, so another page never goes on with the old one.
 */
export function patrolsChanged(): void {
  patrolGen++;
  patrolRead = null;
  if (patrolListeners.size > 0) void readPatrols();
}

export function patrolConversationIds(): Promise<Set<string>> {
  return readPatrols().then((list) => new Set((list ?? []).map((p) => p.conversation_id ?? "").filter(Boolean)));
}

function subscribePatrols(listener: () => void): () => void {
  patrolListeners.add(listener);
  return () => patrolListeners.delete(listener);
}

/** usePatrols: the caller's patrols once known (empty until then, or when `enabled` is false). */
export function usePatrols(enabled: boolean): readonly PatrolBrief[] {
  const list = useSyncExternalStore(subscribePatrols, () => patrolList, () => NO_PATROLS);
  useEffect(() => {
    if (enabled) void readPatrols();
  }, [enabled]);
  return enabled ? list : NO_PATROLS;
}

/**
 * patrolsByConversation: each patrol chat's one patrol to show on its row —
 * the active one that runs soonest, or a paused one when none is active.
 */
export function patrolsByConversation(list: readonly PatrolBrief[]): Map<string, PatrolBrief> {
  const out = new Map<string, PatrolBrief>();
  const rank = (p: PatrolBrief) => (p.status === "active" ? (p.next_run_at ?? Number.MAX_SAFE_INTEGER) : Infinity);
  for (const p of list) {
    if (!p.conversation_id) continue;
    const cur = out.get(p.conversation_id);
    if (!cur || rank(p) < rank(cur)) out.set(p.conversation_id, p);
  }
  return out;
}

/** usePatrolIds: the patrol chats' ids once known (empty until then, or when `enabled` is false). */
export function usePatrolIds(enabled: boolean): Set<string> {
  const list = usePatrols(enabled);
  return useMemo(() => new Set(list.map((p) => p.conversation_id).filter(Boolean)), [list]);
}

/** Test hook. */
export function resetPatrolCacheForTests() {
  patrolRead = null;
  patrolGen = 0;
  patrolReadDone = false;
  patrolList = NO_PATROLS;
}

// ── Pages other than Chat ─────────────────────────────────────────────────
// Chat provides SidebarTargetsContext from the lists it already holds. On
// every other page (Background tasks, Settings, Guides, Analytics) the rail
// reads the same lists itself — once per page, and only when it has a chat or
// project shortcut — so it can show live names and, above all, never show a
// workspace-default shortcut this account cannot open. Opening still
// navigates to Chat (open returns false).

type Named = { id: string; title?: string; name?: string };
// Each list is kept on its own: one that failed leaves only the targets it
// could have answered "unknown", never hides a target another list found.
type Lists = {
  chats: Map<string, string>;
  /** Every chat list (own, archived, shared) answered: a miss is a real "no". */
  chatsComplete: boolean;
  projects: Map<string, string> | null;
};
let listsRead: Promise<Lists> | null = null;

async function readList(url: string, key: "conversations" | "projects"): Promise<Named[] | null> {
  try {
    const res = await fetch(url, { cache: "no-store" });
    if (!res.ok) return null;
    const data = (await res.json()) as Record<string, Named[] | null | undefined>;
    return data[key] ?? [];
  } catch {
    return null;
  }
}

function readLists(): Promise<Lists> {
  if (!listsRead) {
    listsRead = Promise.all([
      readList("/api/conversations", "conversations"),
      readList("/api/conversations?archived=true", "conversations"),
      readList("/api/conversations?scope=shared", "conversations"),
      readList("/api/projects", "projects"),
    ]).then(([own, archived, shared, projects]) => ({
      chats: new Map([...(own ?? []), ...(archived ?? []), ...(shared ?? [])].map((c) => [c.id, c.title ?? ""])),
      chatsComplete: Boolean(own && archived && shared),
      projects: projects ? new Map(projects.map((p) => [p.id, p.name ?? ""])) : null,
    }));
  }
  return listsRead;
}

function availabilityIn(lists: Lists, item: SidebarItem): Availability {
  const ref = item.ref ?? "";
  if (item.kind === "chat") {
    if (lists.chats.has(ref)) return "yes";
    return lists.chatsComplete ? "no" : "unknown";
  }
  if (item.kind === "project") {
    if (!lists.projects) return "unknown";
    return lists.projects.has(ref) ? "yes" : "no";
  }
  return "unknown";
}

/**
 * canOpen: whether this account can open a chat or project shortcut's
 * target, from the same lists the rail reads (null when that cannot be told:
 * the list that would answer failed). Other kinds are not checked here.
 */
export async function canOpen(item: SidebarItem): Promise<boolean | null> {
  const a = availabilityIn(await readLists(), item);
  return a === "unknown" ? null : a === "yes";
}

/** useFallbackTargets: targets for a page that is not Chat (null until read, or when not needed). */
export function useFallbackTargets(enabled: boolean): SidebarTargets | null {
  const [lists, setLists] = useState<Lists | null>(null);
  useEffect(() => {
    if (!enabled) return;
    let live = true;
    void readLists().then((l) => {
      if (live) setLists(l);
    });
    return () => {
      live = false;
    };
  }, [enabled]);
  if (!enabled || !lists) return null;
  return {
    activeConversationId: null,
    activeProjectId: null,
    titleFor: (item) =>
      (item.kind === "chat" ? lists.chats.get(item.ref ?? "") : item.kind === "project" ? lists.projects?.get(item.ref ?? "") : undefined) ||
      undefined,
    availability: (item) => availabilityIn(lists, item),
    open: () => false,
  };
}

/** Test hook. */
export function resetFallbackTargetsForTests() {
  listsRead = null;
}
