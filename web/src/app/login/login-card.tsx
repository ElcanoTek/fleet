"use client";

import { useEffect, useState } from "react";

function Icon({ name, className }: { name: string; className?: string }) {
  return (
    <svg className={className} aria-hidden="true">
      <use href={`/icons/core-icons.svg#${name}`} />
    </svg>
  );
}

// errorCodeToMessage maps the `?e=` query param our login handler redirects
// with to a human-readable message. We keep "invalid" deliberately vague
// so the UI can't be used to enumerate which email addresses exist.
function errorCodeToMessage(code: string | null): string | null {
  if (!code) return null;
  if (code === "invalid") return "Invalid email or password.";
  if (code === "missing") return "Please enter both email and password.";
  if (code === "server") return "The chat server isn't reachable right now. Try again in a moment.";
  if (code === "elcano_unavailable")
    return "Elcano email sign-in isn't available right now. Use your email and password.";
  return "Could not sign in.";
}

// elcanoLoginEnabled is resolved server-side from AUTH_SIGNING_PUBKEY (the same
// gate the backend uses) and passed in as a prop. When the Elcano-email path
// isn't configured — e.g. a white-labelled deploy — the secondary button and
// its divider are omitted entirely so the card shows only the password form
// and never surfaces the Elcano brand.
export default function LoginCard({ elcanoLoginEnabled }: { elcanoLoginEnabled: boolean }) {
  const [loginError, setLoginError] = useState<string | null>(null);
  const [theme, setTheme] = useState<"light" | "dark">("dark");

  // Reading the `?e=` query param must happen after hydration — `window` is
  // undefined during SSR, and using the useState lazy initializer would
  // cause a hydrate mismatch for the initial render. A one-shot setState
  // in a mount effect is the recommended pattern here.
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const current = document.documentElement.getAttribute("data-theme");
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setLoginError(errorCodeToMessage(params.get("e")));
    // Read the bootstrapped theme after mount so SSR and hydration start from
    // the same markup.
    if (current === "light" || current === "dark") {
      setTheme(current);
    }
  }, []);

  const toggleTheme = () => {
    const nextTheme = theme === "dark" ? "light" : "dark";
    document.documentElement.setAttribute("data-theme", nextTheme);
    window.localStorage.setItem("chat-theme-preference", nextTheme);
    setTheme(nextTheme);
  };

  return (
    <main className="flex min-h-screen items-center justify-center bg-[var(--gradient-bg-home-signature)] px-6 py-10">
      <div className="w-full max-w-sm rounded-[1.5rem] border border-[var(--color-border)] bg-[var(--composer-surface)] p-6 shadow-[var(--composer-shadow)]">
        <div className="mb-6 flex items-start justify-between gap-4">
          <div className="grid gap-2">
            <h1 className="text-[1.25rem] font-semibold text-[var(--color-text-primary)]">Welcome aboard.</h1>
            <p className="text-[0.875rem] text-[var(--color-text-secondary)]">
              Sign in to your workspace and pick up where you left off.
            </p>
          </div>

          <button
            aria-label={theme === "dark" ? "Switch to light mode" : "Switch to dark mode"}
            aria-pressed={theme === "dark"}
            className="inline-flex size-9 shrink-0 items-center justify-center rounded-full border border-[var(--color-border)] text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]"
            type="button"
            onClick={toggleTheme}
          >
            <span className="relative size-4" aria-hidden="true">
              <Icon
                name="sun"
                className={[
                  "absolute inset-0 size-4 transition duration-200",
                  theme === "light" ? "rotate-0 scale-100 opacity-100" : "-rotate-12 scale-[0.86] opacity-0",
                ].join(" ")}
              />
              <Icon
                name="moon"
                className={[
                  "absolute inset-0 size-4 transition duration-200",
                  theme === "dark" ? "rotate-0 scale-100 opacity-100" : "rotate-12 scale-[0.86] opacity-0",
                ].join(" ")}
              />
            </span>
          </button>
        </div>

        {loginError ? (
          <div className="mb-4 rounded-xl border border-[#e08080] bg-[color-mix(in_srgb,#e08080_14%,transparent)] px-3 py-2 text-[0.8125rem] text-[#e08080]">
            {loginError}
          </div>
        ) : null}

        <form action="/api/auth/login" method="post" className="grid gap-4">
          <label htmlFor="email" className="grid gap-1.5 text-[0.8125rem] text-[var(--color-text-secondary)]">
            Email
            <input
              id="email"
              name="email"
              type="email"
              autoComplete="email"
              required
              className="rounded-xl border border-[var(--color-border)] bg-transparent px-3 py-2.5 text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
            />
          </label>

          <label htmlFor="password" className="grid gap-1.5 text-[0.8125rem] text-[var(--color-text-secondary)]">
            Password
            <input
              id="password"
              name="password"
              type="password"
              autoComplete="current-password"
              required
              className="rounded-xl border border-[var(--color-border)] bg-transparent px-3 py-2.5 text-[var(--color-text-primary)] outline-none focus:border-[var(--color-accent)]"
            />
          </label>

          <button
            type="submit"
            className="mt-2 rounded-xl bg-[var(--color-primary)] px-4 py-2.5 text-sm font-medium text-white transition hover:opacity-90"
          >
            Sign in
          </button>
        </form>

        {elcanoLoginEnabled ? (
          <>
            <div className="my-5 flex items-center gap-3 text-[0.6875rem] uppercase tracking-wide text-[var(--color-text-muted)]">
              <span className="h-px flex-1 bg-[var(--color-border)]" />
              or
              <span className="h-px flex-1 bg-[var(--color-border)]" />
            </div>

            {/* Secondary sign-in: hands off to the auth service's magic-link flow
                (Pattern B). Kept visually subordinate to the primary password
                action above, per the flag design system's primary-semantics rule. */}
            <a
              href="/api/auth/elcano-login"
              className="flex items-center justify-center rounded-xl border border-[var(--color-border)] px-4 py-2.5 text-sm font-medium text-[var(--color-text-primary)] transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]"
            >
              Use Elcano email
            </a>
          </>
        ) : null}
      </div>
    </main>
  );
}
