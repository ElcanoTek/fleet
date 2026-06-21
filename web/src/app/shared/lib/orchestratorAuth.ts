"use client";

// Client-side bearer-token storage for the orchestrator's moc username/password
// login path. Ported from moc's assets/js/auth-session.js (token persistence
// half). The elcano-cookie path needs none of this — the httpOnly cookie rides
// along automatically — so a "cookie session" flag tracks that case so the
// dashboard knows it's signed in even with no token in storage.

const USER_TOKEN_KEY = "orchestratorToken";
const LEGACY_TOKEN_KEY = "userToken"; // moc's original key, migrated on read

function safeStorage(): Storage | null {
  try {
    return typeof window !== "undefined" ? window.localStorage : null;
  } catch {
    return null;
  }
}

export function getStoredToken(): string {
  const ls = safeStorage();
  if (!ls) return "";
  return ls.getItem(USER_TOKEN_KEY) || ls.getItem(LEGACY_TOKEN_KEY) || "";
}

export function setStoredToken(token: string): void {
  const ls = safeStorage();
  if (!ls) return;
  if (token) {
    ls.setItem(USER_TOKEN_KEY, token);
    ls.removeItem(LEGACY_TOKEN_KEY);
  } else {
    ls.removeItem(USER_TOKEN_KEY);
    ls.removeItem(LEGACY_TOKEN_KEY);
  }
}

export function clearStoredToken(): void {
  setStoredToken("");
}
