"use client";

import { useCallback, useEffect, useState } from "react";
import { classifyBootstrapFailure } from "@/app/chat/ui/bootstrapFailure";
import { OrchestratorError, orchestratorApi } from "@/app/shared/lib/orchestratorApi";
import { purgeLegacyOrchestratorTokens } from "@/app/shared/lib/orchestratorAuth";

// useOrchestratorSession owns the orchestrator's login state.
// Cookie/OIDC is the only operator path (the username/password form and
// its localStorage bearer are gone — #1115). /me remains the single
// source of truth; the proxy resolves the httpOnly session cookie.

export type OrchestratorSession = {
  ready: boolean; // initial probe complete
  signedIn: boolean;
  username?: string;
  role?: string; // "admin" | "client" | "readonly"; may be absent for an admin-API-key principal
  noAccess: boolean; // authenticated to chat, but not provisioned in the orchestrator (/me → 403 not_a_member)
  unreachable: boolean; // the probe failed with a non-auth verdict (5xx/network) — backend down, session state unknown
  logout: () => Promise<void>;
};

export function useOrchestratorSession(): OrchestratorSession {
  const [ready, setReady] = useState(false);
  const [signedIn, setSignedIn] = useState(false);
  const [username, setUsername] = useState<string | undefined>(undefined);
  const [role, setRole] = useState<string | undefined>(undefined);
  const [noAccess, setNoAccess] = useState(false);
  const [unreachable, setUnreachable] = useState(false);

  // Drop leftover moc bearer tokens on first load after upgrade (#1115).
  useEffect(() => {
    purgeLegacyOrchestratorTokens();
  }, []);

  // Initial probe (#458 symptoms 1 + 3): ALWAYS hit /me — the route
  // resolves a valid elcano cookie, so it is the single source of truth
  // for signedIn/username/role. Distinguishes "not signed in" (401) from
  // "signed in but not an orchestrator member" (403 not_a_member).
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const me = await orchestratorApi.me();
        if (cancelled) return;
        if (me?.authenticated) {
          setSignedIn(true);
          setUsername(me.username);
          setRole(me.role);
        }
      } catch (err) {
        if (cancelled) return;
        const status = err instanceof OrchestratorError ? err.status : null;
        if (status === null || classifyBootstrapFailure(status) === "unreachable") {
          // 5xx or a thrown network failure: the backend — or the chat DB the
          // fail-closed epoch check consults (it answers 500, never a
          // revocation, on a lookup error: internal/sched/handlers/
          // session_epoch.go) — is down, which says nothing about the session.
          // Rendering the login card here would invite people to type
          // credentials mid-incident, so surface a distinct unreachable state.
          // Same 401/403-only auth verdict as the chat plane
          // (chat/ui/bootstrapFailure.ts).
          setUnreachable(true);
        } else if (status === 403) {
          // not_a_member: a valid chat-cookie identity with no orchestrator
          // membership. Authenticated, but lacks access — surface a no-access
          // card rather than the login loop.
          setNoAccess(true);
          setSignedIn(false);
        } else {
          // 401 → genuinely not signed in.
          setSignedIn(false);
          setNoAccess(false);
        }
      } finally {
        if (!cancelled) setReady(true);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const logout = useCallback(async () => {
    try {
      await fetch("/api/orchestrator/auth/logout", {
        method: "POST",
        credentials: "same-origin",
      });
    } catch {
      /* best effort */
    }
    purgeLegacyOrchestratorTokens();
    setSignedIn(false);
    setUsername(undefined);
    setRole(undefined);
    setNoAccess(false);
  }, []);

  return { ready, signedIn, username, role, noAccess, unreachable, logout };
}
