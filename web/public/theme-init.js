// Stamps data-theme on <html> BEFORE first paint. Both the globals.css light
// palette AND the bundle's brand palette (/api/theme, whose rules are
// html:root[data-theme=…]) key off that attribute, so until it exists the
// browser paints fleet's default :root colors — a flash on every hard refresh
// of a themed deployment.
//
// It is a classic, parser-blocking <script src> in <head> (index.html), so it
// runs before any body content exists to paint. It is a FILE rather than an
// inline script so the page CSP can stay script-src 'self'.
//
// Keep the storage key + fallback in sync with useTheme.ts, which owns the
// same contract after mount (THEME_STORAGE_KEY there).
(() => {
  const storageKey = "chat-theme-preference";
  const root = document.documentElement;
  try {
    const stored = window.localStorage.getItem(storageKey);
    const theme =
      stored === "light" || stored === "dark"
        ? stored
        : window.matchMedia("(prefers-color-scheme: dark)").matches
          ? "dark"
          : "light";
    root.setAttribute("data-theme", theme);
  } catch {
    root.setAttribute("data-theme", "dark");
  }
})();
