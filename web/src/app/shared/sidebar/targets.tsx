import { createContext, useContext, useEffect, useState } from "react";
import type { SidebarItem } from "./layout";

// What the surface knows about the things a sidebar shortcut points at. Chat
// provides it (it holds the conversations, team chats and projects): live
// names, which one is open, whether this account can still open a target, and
// an in-page opener so a click does not reload the app. Every other surface
// renders without it — shortcuts there show the name saved with them and
// navigate to Chat.

export type Availability = "yes" | "no" | "unknown";

export type SidebarTargets = {
  activeConversationId: string | null;
  activeProjectId: string | null;
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

// ── Patrols: which conversations are patrol chats (for their icon and kind) ──
// One read of GET /api/patrols per page, shared, and only when something asks.

let patrolRead: Promise<Set<string>> | null = null;

export function patrolConversationIds(): Promise<Set<string>> {
  if (!patrolRead) {
    patrolRead = fetch("/api/patrols", { cache: "no-store" })
      .then(async (res) => {
        if (!res.ok) return new Set<string>();
        const data = (await res.json()) as { patrols?: { conversation_id?: string }[] };
        return new Set((data.patrols ?? []).map((p) => p.conversation_id ?? "").filter(Boolean));
      })
      .catch(() => new Set<string>());
  }
  return patrolRead;
}

/** usePatrolIds: the patrol chats' ids once known (empty until then, or when `enabled` is false). */
export function usePatrolIds(enabled: boolean): Set<string> {
  const [ids, setIds] = useState<Set<string>>(() => new Set());
  useEffect(() => {
    if (!enabled) return;
    let live = true;
    void patrolConversationIds().then((s) => {
      if (live) setIds(s);
    });
    return () => {
      live = false;
    };
  }, [enabled]);
  return ids;
}

/** Test hook. */
export function resetPatrolCacheForTests() {
  patrolRead = null;
}

// ── Pages other than Chat ─────────────────────────────────────────────────
// Chat provides SidebarTargetsContext from the lists it already holds. On
// every other page (Background tasks, Settings, Guides, Analytics) the rail
// reads the same lists itself — once per page, and only when it has a chat or
// project shortcut — so it can show live names and, above all, never show a
// workspace-default shortcut this account cannot open. Opening still
// navigates to Chat (open returns false).

type Named = { id: string; title?: string; name?: string };
type Lists = { chats: Map<string, string>; projects: Map<string, string> };
let listsRead: Promise<Lists | null> | null = null;

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

function readLists(): Promise<Lists | null> {
  if (!listsRead) {
    listsRead = Promise.all([
      readList("/api/conversations", "conversations"),
      readList("/api/conversations?archived=true", "conversations"),
      readList("/api/conversations?scope=shared", "conversations"),
      readList("/api/projects", "projects"),
    ]).then(([own, archived, shared, projects]) => {
      // Any list that failed leaves availability unknown rather than "no".
      if (!own || !archived || !shared || !projects) return null;
      return {
        chats: new Map([...own, ...archived, ...shared].map((c) => [c.id, c.title ?? ""])),
        projects: new Map(projects.map((p) => [p.id, p.name ?? ""])),
      };
    });
  }
  return listsRead;
}

/**
 * canOpen: whether this account can open a chat or project shortcut's
 * target, from the same lists the rail reads (null when they could not be
 * read). Other kinds are not checked here.
 */
export async function canOpen(item: SidebarItem): Promise<boolean | null> {
  const lists = await readLists();
  if (!lists) return null;
  if (item.kind === "chat") return lists.chats.has(item.ref ?? "");
  if (item.kind === "project") return lists.projects.has(item.ref ?? "");
  return null;
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
      (item.kind === "chat" ? lists.chats.get(item.ref ?? "") : item.kind === "project" ? lists.projects.get(item.ref ?? "") : undefined) ||
      undefined,
    availability: (item) => {
      if (item.kind === "chat") return lists.chats.has(item.ref ?? "") ? "yes" : "no";
      if (item.kind === "project") return lists.projects.has(item.ref ?? "") ? "yes" : "no";
      return "unknown";
    },
    open: () => false,
  };
}

/** Test hook. */
export function resetFallbackTargetsForTests() {
  listsRead = null;
}
