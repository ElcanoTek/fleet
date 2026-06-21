"use client";

import { useCallback, useEffect, useState } from "react";
import { orchestratorApi } from "@/app/shared/lib/orchestratorApi";
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
  login: (username: string, password: string) => Promise<boolean>;
  logout: () => Promise<void>;
  error: string | null;
};

export function useOrchestratorSession(): OrchestratorSession {
  const [ready, setReady] = useState(false);
  const [signedIn, setSignedIn] = useState(false);
  const [username, setUsername] = useState<string | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);

  // Initial probe: a stored bearer token, or a valid elcano cookie session.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      if (getStoredToken()) {
        if (!cancelled) {
          setSignedIn(true);
          setReady(true);
        }
        return;
      }
      try {
        const me = await orchestratorApi.me();
        if (!cancelled && me?.authenticated) {
          setSignedIn(true);
          setUsername(me.username);
        }
      } catch {
        /* not signed in */
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
  }, []);

  return { ready, signedIn, username, login, logout, error };
}
