// Recovering from a failed code-split load.
//
// A tab that outlives a deploy still runs the old bundle, and the first time it
// needs a piece it has not loaded yet (a lazily imported view, a route's chunk)
// that load can fail: the build it belongs to may be gone, or the network may
// have dropped mid-request. React then throws into the nearest error boundary.
// A reload fetches the current page and its current chunks, which is the fix,
// so the boundaries in app/error.tsx and app/global-error.tsx do it for the
// reader, ONCE. If the reload lands on the same failure the boundary shows its
// Reload button instead of reloading in a loop.

// AUTO_RELOAD_WINDOW_MS is how recently an automatic reload counts as "this
// same failure again". A page that fails again inside it is not reloaded; one
// that fails later (the next deploy, a day on) gets its own single retry.
export const AUTO_RELOAD_WINDOW_MS = 2 * 60 * 1000;

const STORAGE_KEY = "fleet-chunk-reload-at";

// The ways a failed chunk or dynamic import surfaces across bundlers and
// browsers. Turbopack and webpack name their error ChunkLoadError ("Failed to
// load chunk …" / "Loading chunk … failed"); a native dynamic import() that
// fails is a plain TypeError whose message is per-browser.
const CHUNK_ERROR_MESSAGES = [
  /failed to load chunk/i,
  /loading (?:css )?chunk [^ ]+ failed/i,
  /failed to fetch dynamically imported module/i, // Chromium
  /importing a module script failed/i, // WebKit (Safari, every iOS browser)
  /error loading dynamically imported module/i, // Firefox
];

// isChunkLoadError reports whether an error is a failed chunk or dynamic import.
export function isChunkLoadError(error: unknown): boolean {
  if (!(error instanceof Error)) return false;
  if (error.name === "ChunkLoadError") return true;
  return CHUNK_ERROR_MESSAGES.some((re) => re.test(error.message));
}

// claimChunkAutoReload records an automatic reload and says whether one may
// happen now: false when another happened inside AUTO_RELOAD_WINDOW_MS, and
// false when session storage is unavailable, because then nothing could stop a
// loop.
export function claimChunkAutoReload(now: number = Date.now()): boolean {
  try {
    const last = Number(window.sessionStorage.getItem(STORAGE_KEY));
    if (Number.isFinite(last) && last > 0 && now - last < AUTO_RELOAD_WINDOW_MS) {
      return false;
    }
    window.sessionStorage.setItem(STORAGE_KEY, String(now));
    return true;
  } catch {
    return false;
  }
}
