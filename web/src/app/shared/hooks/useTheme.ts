"use client";

import { useCallback, useEffect, useState } from "react";

// useTheme is the single source of truth for the app's light/dark theme. The
// chat view, the orchestrator, and the login card each used to hand-roll this
// same logic; consolidating it here keeps the shared shell's theme behavior
// identical across every view.
//
// Contract (persisted prefs and the inline pre-paint bootstrap in layout.tsx
// keep working):
//   - An explicit preference is stored under the "chat-theme-preference"
//     localStorage key as the literal string "light" or "dark".
//   - "System" is the absence of a stored preference: the OS
//     `prefers-color-scheme` is followed and tracked live via matchMedia.
//     Choosing System removes the stored key; choosing Light/Dark persists
//     it and stops the OS tracking. Default with nothing stored is System.
//   - The resolved theme is applied to <html data-theme="…">, which the
//     globals.css / brand-palette CSS variables key off.
//
// The first paint is handled by the inline theme-init script in layout.tsx
// (synchronous in <head>, so it runs before anything paints), and this hook's
// mount effect only syncs React state to the attribute that script already
// set — there is no theme flash.

export const THEME_STORAGE_KEY = "chat-theme-preference";

export type Theme = "light" | "dark";
// What the user picked — "system" means "no stored preference, follow the OS".
export type ThemePreference = Theme | "system";

function readStoredTheme(): Theme | null {
  try {
    const stored = window.localStorage.getItem(THEME_STORAGE_KEY);
    return stored === "light" || stored === "dark" ? stored : null;
  } catch {
    return null;
  }
}

function systemTheme(): Theme {
  // matchMedia is absent in some non-browser runtimes (older jsdom, SSR
  // shims). Default to dark — mirrors the catch fallback in layout.tsx's
  // inline theme-init script.
  if (typeof window.matchMedia !== "function") return "dark";
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

export type UseTheme = {
  /** The resolved theme driving the UI — never "system". */
  theme: Theme;
  /** The user's choice — "system" when no explicit preference is stored. */
  themePreference: ThemePreference;
  toggleTheme: () => void;
  /** Set a preference — used by the account menu's System/Light/Dark segmented control. */
  setTheme: (next: ThemePreference) => void;
  /** "Switch to light mode" / "Switch to dark mode" — for the toggle's label. */
  themeLabel: string;
};

export function useTheme(): UseTheme {
  // Defaults to "dark"/"system" to match the pre-paint default in layout.tsx's
  // inline theme-init script; the mount effect below reconciles to the real
  // values synchronously after hydration so SSR markup never mismatches.
  const [theme, setThemeState] = useState<Theme>("dark");
  const [themePreference, setThemePreference] = useState<ThemePreference>("system");

  useEffect(() => {
    const root = document.documentElement;
    const media =
      typeof window.matchMedia === "function"
        ? window.matchMedia("(prefers-color-scheme: dark)")
        : null;

    const applyTheme = (next: Theme) => {
      root.setAttribute("data-theme", next);
      setThemeState(next);
    };

    const reconcileFromStorage = () => {
      const stored = readStoredTheme();
      setThemePreference(stored ?? "system");
      applyTheme(stored ?? (media?.matches ? "dark" : "light"));
    };
    reconcileFromStorage();

    if (!media) return;

    // Follow the OS theme while no explicit preference is stored. The check
    // runs per-event, so clearing the preference (choosing System) resumes
    // tracking without re-arming the listener.
    const handleSystemChange = () => {
      if (readStoredTheme()) return;
      applyTheme(media.matches ? "dark" : "light");
    };
    media.addEventListener("change", handleSystemChange);
    return () => media.removeEventListener("change", handleSystemChange);
  }, []);

  // setTheme writes the choice through to the live attribute and the persisted
  // preference. "system" clears the stored key and re-resolves from the OS;
  // the mount listener above then keeps tracking OS changes live.
  const setTheme = useCallback((next: ThemePreference) => {
    setThemePreference(next);
    if (next === "system") {
      try {
        window.localStorage.removeItem(THEME_STORAGE_KEY);
      } catch {
        // Private-mode / storage-disabled: still re-resolve the live attribute.
      }
      const resolved = systemTheme();
      setThemeState(resolved);
      document.documentElement.setAttribute("data-theme", resolved);
      return;
    }
    setThemeState(next);
    document.documentElement.setAttribute("data-theme", next);
    try {
      window.localStorage.setItem(THEME_STORAGE_KEY, next);
    } catch {
      // Private-mode / storage-disabled: still flip the live attribute.
    }
  }, []);

  // The sun/moon toggle (login card, orchestrator slim header) flips to an
  // explicit theme — same persistence path as picking Light/Dark.
  const toggleTheme = useCallback(() => {
    setThemeState((prev) => {
      const next: Theme = prev === "dark" ? "light" : "dark";
      setThemePreference(next);
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

  return { theme, themePreference, toggleTheme, setTheme, themeLabel };
}

export default useTheme;
