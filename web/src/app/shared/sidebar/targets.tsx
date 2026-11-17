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
