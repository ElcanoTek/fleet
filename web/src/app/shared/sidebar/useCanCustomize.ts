import { useEffect, useState } from "react";
import { fetchMe } from "@/app/shared/lib/account";

// useCanCustomize: whether this account may save a sidebar layout. The API
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
  return role !== null && role !== "" && role !== "viewer";
}
