// useSessionEmail — the signed-in email for a page shell's account control
// (#2003).
//
// Go renders the session's email into the page bootstrap on every request,
// so the shell normally has it before the first paint and makes no request.
// Only when the bootstrap carries no session (a dev server in front of an
// older binary, a test harness) is /api/session asked; a 401 there means the
// session is gone and the page goes to /login. A failed read leaves the email
// empty: the account button shows its loading state, and every page-level
// fetch still handles its own auth redirect.

import { useEffect, useState } from "react";
import { getBootstrap } from "@/app/lib/bootstrap";

export function useSessionEmail(): string {
  const [email, setEmail] = useState(() => getBootstrap().session?.email ?? "");
  const seeded = email !== "";

  useEffect(() => {
    if (seeded) return;
    let stale = false;
    void (async () => {
      try {
        const res = await fetch("/api/session", { cache: "no-store" });
        if (res.status === 401) {
          window.location.href = "/login";
          return;
        }
        if (!res.ok) return;
        const data = (await res.json()) as { email?: string };
        if (!stale && data.email) setEmail(data.email);
      } catch {
        // Transient failure: the email stays empty (see above).
      }
    })();
    return () => {
      stale = true;
    };
    // Runs at most once: a seeded email never fetches, and a fetched one
    // flips `seeded`, whose re-run returns at once.
  }, [seeded]);

  return email;
}
