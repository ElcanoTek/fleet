"use client";

import { useCallback, useEffect, useState } from "react";
import { OrchestratorError, orchestratorApi } from "@/app/shared/lib/orchestratorApi";
import {
  clearStoredToken,
  getStoredToken,
  setStoredToken,
} from "@/app/shared/lib/orchestratorAuth";

// useOrchestratorSession owns the orchestrator's TWO-path login state:
//   - moc bearer  (username/password) → token in localStorage.
//   - elcano cookie ("Use Elcano email") → no token; detected by probing
//     /api/orchestrator/me, which succeeds for any valid credential.
// Mirrors moc's auth-session.js isAuthenticated()/cookie-session probe.

export type OrchestratorSession = {
  ready: boolean; // initial probe complete
  signedIn: boolean;
  username?: string;
  role?: string; // "admin" | "client" | "readonly"; may be absent for an admin-API-key principal
  noAccess: boolean; // authenticated to chat, but not provisioned in the orchestrator (/me → 403 not_a_member)
  login: (username: string, password: string) => Promise<boolean>;
  logout: () => Promise<void>;
  error: string | null;
};

export function useOrchestratorSession(): OrchestratorSession {
  const [ready, setReady] = useState(false);
  const [signedIn, setSignedIn] = useState(false);
  const [username, setUsername] = useState<string | undefined>(undefined);
  const [role, setRole] = useState<string | undefined>(undefined);
  const [noAccess, setNoAccess] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Initial probe (#458 symptoms 1 + 3): ALWAYS hit /me — the route resolves a
  // stored bearer OR a valid elcano cookie, so it is the single source of truth
  // for signedIn/username/role. Probing unconditionally fixes the old early
  // return that left username unset (account menu stuck on "Loading…") on a
  // bearer reload, and it distinguishes "not signed in" (401) from
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
        if (err instanceof OrchestratorError && err.status === 403) {
          // not_a_member: a valid chat-cookie identity with no orchestrator
          // membership. Authenticated, but lacks access — surface a no-access
          // card rather than the login loop.
          setNoAccess(true);
          setSignedIn(false);
        } else {
          // 401 or any other failure → genuinely not signed in.
          setSignedIn(false);
          setNoAccess(false);
          // Self-heal a stale bearer (#458 symptom 3): an invalid/expired token
          // would otherwise keep shadowing a valid cookie session on every
          // request. Clear it so the next probe falls back to the cookie.
          if (err instanceof OrchestratorError && err.status === 401) {
            clearStoredToken();
          }
        }
      } finally {
        if (!cancelled) setReady(true);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const login = useCallback(async (user: string, password: string): Promise<boolean> => {
    setError(null);
    if (!user || !password) {
      setError("Please enter username and password");
      return false;
    }
    try {
      const res = await fetch("/api/orchestrator/auth/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username: user, password }),
      });
      if (!res.ok) {
        const body = await res.json().catch(() => ({ detail: "Login failed" }));
        setError(body.detail || "Invalid credentials");
        return false;
      }
      const data = await res.json();
      if (!data.token) {
        setError("No token received from server");
        return false;
      }
      setStoredToken(data.token);
      setSignedIn(true);
      setUsername(data.user?.username);
      setRole(data.user?.role);
      return true;
    } catch (err) {
      setError((err as Error).message || "Login failed");
      return false;
    }
  }, []);

  const logout = useCallback(async () => {
    try {
      const token = getStoredToken();
      await fetch("/api/orchestrator/auth/logout", {
        method: "POST",
        headers: token ? { Authorization: `Bearer ${token}` } : {},
      });
    } catch {
      /* best effort */
    }
    clearStoredToken();
    setSignedIn(false);
    setUsername(undefined);
    setRole(undefined);
    setNoAccess(false);
  }, []);

  return { ready, signedIn, username, role, noAccess, login, logout, error };
}
