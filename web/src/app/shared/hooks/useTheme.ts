"use client";

import { useCallback, useEffect, useState } from "react";

// useTheme is the single source of truth for the app's light/dark theme. The
// chat view, the orchestrator, and the login card each used to hand-roll this
// same logic; consolidating it here keeps the shared shell's theme behavior
// identical across every view.
//
// Contract (unchanged from the prior inline copies, so persisted prefs and the
// render-blocking /scripts/theme.js bootstrap keep working):
//   - The preference is stored under the "chat-theme-preference" localStorage
//     key as the literal string "light" or "dark".
//   - The resolved theme is applied to <html data-theme="…">, which the
//     globals.css / brand-palette CSS variables key off.
//   - With no stored preference, the OS `prefers-color-scheme` is followed and
//     tracked live until the user makes an explicit choice.
//
// The first paint is handled before hydration by /scripts/theme.js (wired in
// layout.tsx), so this hook's mount effect only syncs React state to the
// attribute that script already set — there is no theme flash.

export const THEME_STORAGE_KEY = "chat-theme-preference";

export type Theme = "light" | "dark";

function readStoredTheme(): Theme | null {
  try {
    const stored = window.localStorage.getItem(THEME_STORAGE_KEY);
    return stored === "light" || stored === "dark" ? stored : null;
  } catch {
    return null;
  }
}

export type UseTheme = {
  theme: Theme;
  toggleTheme: () => void;
  /** "Switch to light mode" / "Switch to dark mode" — for the toggle's label. */
  themeLabel: string;
};

export function useTheme(): UseTheme {
  // Defaults to "dark" to match the pre-hydration default in
  // /scripts/theme.js; the mount effect below reconciles to the real value
  // synchronously after hydration so SSR markup never mismatches.
  const [theme, setTheme] = useState<Theme>("dark");

  useEffect(() => {
    const root = document.documentElement;
    // matchMedia is absent in some non-browser runtimes (older jsdom, SSR
    // shims). Guard so the hook degrades to "stored-or-dark" instead of
    // throwing — mirrors the catch fallback in /scripts/theme.js.
    const media =
      typeof window.matchMedia === "function"
        ? window.matchMedia("(prefers-color-scheme: dark)")
        : null;

    const resolveTheme = (): Theme =>
      readStoredTheme() ?? (media?.matches ? "dark" : "light");

    const applyTheme = (next: Theme) => {
      root.setAttribute("data-theme", next);
      setTheme(next);
    };

    applyTheme(resolveTheme());

    if (!media) return;

    // Follow the OS theme until the user makes an explicit choice.
    const handleSystemChange = () => {
      if (readStoredTheme()) return;
      applyTheme(media.matches ? "dark" : "light");
    };
    media.addEventListener("change", handleSystemChange);
    return () => media.removeEventListener("change", handleSystemChange);
  }, []);

  const toggleTheme = useCallback(() => {
    setTheme((prev) => {
      const next: Theme = prev === "dark" ? "light" : "dark";
      document.documentElement.setAttribute("data-theme", next);
      try {
        window.localStorage.setItem(THEME_STORAGE_KEY, next);
      } catch {
        // Private-mode / storage-disabled: still flip the live attribute.
      }
      return next;
    });
  }, []);

  const themeLabel = theme === "dark" ? "Switch to light mode" : "Switch to dark mode";

  return { theme, toggleTheme, themeLabel };
}

export default useTheme;
