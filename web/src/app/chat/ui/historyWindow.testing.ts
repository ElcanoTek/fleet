// Test support for the windowed conversation load
// (docs/CHAT-HISTORY-WINDOW.md). Not imported by the app.

/**
 * The conversation read's URL without its history-window query, so a test's
 * fake backend matches the conversation it serves whichever part of the
 * history the client asked for (`?limit=N` on a first open, `?from=ID` on a
 * re-read). Any other URL or query — a "Load earlier" page (`before`),
 * `omit_history`, a sub-route — is returned unchanged.
 */
export function withoutHistoryWindow(url: string): string {
  return url.replace(/^(\/api\/conversations\/[A-Za-z0-9_-]+)\?(?:limit=\d+|from=\d+)$/, "$1");
}
