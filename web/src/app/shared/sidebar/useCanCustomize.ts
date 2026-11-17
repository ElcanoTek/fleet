import { useEffect, useState } from "react";
import { fetchMe } from "@/app/shared/lib/account";
import { useSidebar } from "./store";

// useCanCustomize: whether this account may change its sidebar now. Two
// things must be known first. The person's saved layout must have loaded
// (status "ready"): until then the rail shows a cached or built-in stand-in,
// and an edit made on it would be saved over the real one. And the role: the API
// refuses writes from a read-only Viewer (the `mutate` gate), so the editor,
// Reset and the quick pins are offered only once GET /api/me has confirmed a
// role other than "viewer" — the same rule the rail's chat reordering
// follows. While the role is unknown (loading or unreadable) nothing is
// offered, rather than offering edits that would then fail to save.
export function useCanCustomize(): boolean {
  const [role, setRole] = useState<string | null>(null);
  useEffect(() => {
    let live = true;
    void fetchMe().then((me) => {
      if (live) setRole(me?.role ?? null);
    });
    return () => {
      live = false;
    };
  }, []);
  const { status } = useSidebar();
  return status === "ready" && role !== null && role !== "" && role !== "viewer";
}
