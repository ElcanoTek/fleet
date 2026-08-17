"use client";

// The orchestrator used to persist moc's username/password bearer in
// localStorage (`orchestratorToken`, plus moc's original `userToken`).
// That path is retired (#1115): the password form is gone, and anything
// in localStorage is readable by XSS. Auth now rides the same httpOnly
// cookie session as chat. This module's only job is a one-time cleanup
// that drops leftover tokens on first load after upgrade.

const LEGACY_TOKEN_KEYS = ["orchestratorToken", "userToken"] as const;

function safeStorage(): Storage | null {
  try {
    return typeof window !== "undefined" ? window.localStorage : null;
  } catch {
    return null;
  }
}

/** Drop leftover moc bearer tokens. Safe to call repeatedly. */
export function purgeLegacyOrchestratorTokens(): void {
  for (const store of [safeStorage(), safeSessionStorage()]) {
    if (!store) continue;
    for (const key of LEGACY_TOKEN_KEYS) {
      store.removeItem(key);
    }
  }
}

function safeSessionStorage(): Storage | null {
  try {
    return typeof window !== "undefined" ? window.sessionStorage : null;
  } catch {
    return null;
  }
}
