"use client";

import { useEffect } from "react";
import { THEME_STORAGE_KEY } from "@/app/shared/hooks/useTheme";
import { ErrorScreen } from "@/app/shared/ui/ErrorScreen";
import "./globals.css";

// The error boundary of last resort: a throw in the root layout itself. It
// REPLACES the root layout, so it brings its own <html>/<body>, the global
// stylesheet, and the bundle's brand palette (/api/theme is public, so it
// resolves even when the session is what broke).
//
// The layout's inline theme-init script is not here, so the light/dark choice
// is applied after mount instead (the same storage key and OS fallback). A
// frame of the default palette on an error screen is an acceptable price for
// not duplicating that script.
export default function GlobalError({ error }: { error: Error & { digest?: string } }) {
  useEffect(() => {
    let theme: "light" | "dark" = "dark";
    try {
      const stored = window.localStorage.getItem(THEME_STORAGE_KEY);
      if (stored === "light" || stored === "dark") {
        theme = stored;
      } else if (typeof window.matchMedia === "function") {
        theme = window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
      }
    } catch {
      // Storage blocked: keep the dark default, as the layout's script does.
    }
    document.documentElement.setAttribute("data-theme", theme);
  }, []);

  return (
    <html lang="en" className="h-full antialiased" suppressHydrationWarning>
      <head>
        <title>Something went wrong</title>
        {/* Same runtime brand palette link as the root layout; see the comment
            there for why it is a <link> and not bundled CSS. */}
        {/* eslint-disable-next-line @next/next/no-css-tags */}
        <link rel="stylesheet" href="/api/theme" />
      </head>
      <body className="min-h-full flex flex-col">
        <ErrorScreen error={error} />
      </body>
    </html>
  );
}
