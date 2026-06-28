"use client";

import { Icon } from "./Icon";
import { useTheme } from "@/app/shared/hooks/useTheme";

// ThemeToggle is the shared sun/moon light-dark switch. The chat header and the
// login card rendered byte-identical copies of this button; it now lives once
// here and is reused across the shared shell (chat, orchestrator, login).
//
// The button chrome (size/shape) differs slightly between surfaces, so the
// wrapper `className` is overridable. The default matches the chat header's
// square icon-button so the most common call site needs no override. The inner
// sun/moon crossfade and the aria semantics are fixed — they are the shared,
// load-bearing part.

const DEFAULT_BUTTON_CLASS =
  "inline-flex size-11 items-center justify-center rounded-md text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:shadow-[var(--focus-ring)] sm:size-8";

export function ThemeToggle({ className }: { className?: string }) {
  const { theme, toggleTheme, themeLabel } = useTheme();

  return (
    <button
      aria-label={themeLabel}
      aria-pressed={theme === "dark"}
      className={className ?? DEFAULT_BUTTON_CLASS}
      title="Toggle theme"
      type="button"
      onClick={toggleTheme}
    >
      <span className="relative size-4" aria-hidden="true">
        <Icon
          name="sun"
          className={[
            "absolute inset-0 size-4 transition duration-200",
            theme === "light"
              ? "rotate-0 scale-100 opacity-100"
              : "-rotate-12 scale-[0.86] opacity-0",
          ].join(" ")}
        />
        <Icon
          name="moon"
          className={[
            "absolute inset-0 size-4 transition duration-200",
            theme === "dark"
              ? "rotate-0 scale-100 opacity-100"
              : "rotate-12 scale-[0.86] opacity-0",
          ].join(" ")}
        />
      </span>
    </button>
  );
}

export default ThemeToggle;
