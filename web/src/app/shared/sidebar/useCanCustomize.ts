import { useEffect, useState } from "react";
import { fetchMe } from "@/app/shared/lib/account";
import { useSidebar, useSidebarCustomizerOpen } from "./store";

// useCanCustomize: whether this account may change its sidebar now. Two
// things must be known first. The person's saved layout must have loaded
// (status "ready"): until then the rail shows a cached or built-in stand-in,
// and an edit made on it would be saved over the real one. And the role: the
// API refuses writes from a read-only Viewer (the `mutate` gate), so the
// editor, Reset and the quick pins are offered only once GET /api/me has
// confirmed a role other than "viewer" — the same rule the rail's chat
// reordering follows.
//
// An unreadable role is not a Viewer: `account` says which it is ("unknown"
// while unread, "failed" when the read failed), and a failed read is tried
// again whenever the Customize panel is opened (or `retry` is called), so a
// passing network blip never leaves the sidebar locked for the whole page.

export type AccountState = "checking" | "failed" | "viewer" | "member";

export function useCustomizeAccess(): { canCustomize: boolean; account: AccountState; retry: () => void } {
  const { status } = useSidebar();
  const panelOpen = useSidebarCustomizerOpen();
  // undefined: checking; null: the read failed; a string: the role.
  const [role, setRole] = useState<string | null | undefined>(undefined);
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    if (typeof role === "string") return;
    let live = true;
    void fetchMe({ fresh: role === null }).then((me) => {
      if (live) setRole(me && me.role ? me.role : null);
    });
    return () => {
      live = false;
    };
    // panelOpen and attempt are the retry triggers.
  }, [role, panelOpen, attempt]);
  const account: AccountState =
    role === undefined ? "checking" : role === null ? "failed" : role === "viewer" ? "viewer" : "member";
  return {
    canCustomize: status === "ready" && account === "member",
    account,
    retry: () => setAttempt((n) => n + 1),
  };
}

export function useCanCustomize(): boolean {
  return useCustomizeAccess().canCustomize;
}
