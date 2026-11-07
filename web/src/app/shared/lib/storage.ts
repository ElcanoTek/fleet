// storage — the one guarded way the UI reads and writes a browser-storage
// preference (#2002).
//
// Web Storage can throw on every access: Safari's private mode and a full
// quota fail setItem, a "block all cookies" setting makes even the
// `window.localStorage` getter throw a SecurityError, and a sandboxed frame
// has no storage at all. A preference (rail collapsed, show stats, send key)
// is never worth a crashed page, so these helpers swallow every failure: a
// read answers null (the caller's default applies) and a write reports false
// (the choice then lasts for this page only).

export type StorageArea = "local" | "session";

function area(which: StorageArea): Storage | null {
  if (typeof window === "undefined") return null;
  // The getter itself is what throws under a cookie block.
  return which === "session" ? window.sessionStorage : window.localStorage;
}

/** The stored value, or null when unset or when storage is unavailable. */
export function readPref(key: string, which: StorageArea = "local"): string | null {
  try {
    return area(which)?.getItem(key) ?? null;
  } catch {
    return null;
  }
}

/** Stores the value; false when storage refused it (private mode, quota). */
export function writePref(key: string, value: string, which: StorageArea = "local"): boolean {
  try {
    const s = area(which);
    if (!s) return false;
    s.setItem(key, value);
    return true;
  } catch {
    return false;
  }
}

/** Removes the key; a storage failure is ignored. */
export function removePref(key: string, which: StorageArea = "local"): void {
  try {
    area(which)?.removeItem(key);
  } catch {
    // Storage unavailable: nothing was stored to remove.
  }
}

// The "Send on Enter" vs "Send on Ctrl/Cmd+Enter" preference, shared by the
// composer's toggle pill and Settings → General. Defaults to "enter" (the
// muscle-memory default for every major chat UI).
export const SEND_KEY_STORAGE = "fleet.sendKey";

/** True unless the person chose Ctrl/Cmd+Enter (or storage is unavailable). */
export function readSendOnEnter(): boolean {
  return (readPref(SEND_KEY_STORAGE) ?? "enter") === "enter";
}

export function writeSendOnEnter(sendOnEnter: boolean): boolean {
  return writePref(SEND_KEY_STORAGE, sendOnEnter ? "enter" : "ctrl+enter");
}
