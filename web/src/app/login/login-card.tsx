"use client";

import { useEffect, useState } from "react";
import { ThemeToggle } from "@/app/shared/ui/ThemeToggle";

// errorCodeToMessage maps the `?e=` query param our login handler redirects
// with to a human-readable message. We keep "invalid" deliberately vague
// so the UI can't be used to enumerate which email addresses exist.
function errorCodeToMessage(code: string | null): string | null {
  if (!code) return null;
  if (code === "invalid") return "Invalid email or password.";
  if (code === "missing") return "Please enter both email and password.";
  if (code === "server") return "The chat server isn't reachable right now. Try again in a moment.";
  // "in a minute" matches the verify endpoint's Retry-After: 60
  // (internal/httpapi/auth_verify.go).
  if (code === "throttled") return "Too many sign-in attempts. Try again in a minute.";
  if (code === "elcano_unavailable")
    return "Elcano email sign-in isn't available right now. Use your email and password.";
  if (code === "oidc_unavailable")
    return "Single sign-on isn't available right now. Use your email and password.";
  if (code === "oidc_denied") return "Single sign-on was cancelled.";
  if (code === "oidc_domain") return "Your account's email domain isn't allowed to sign in here.";
  if (code === "oidc_error") return "Single sign-on failed. Try again, or use your email and password.";
  return "Could not sign in.";
}

// magicLinkLoginEnabled is resolved server-side from AUTH_SIGNING_PUBKEY (the same
// gate the backend uses) and passed in as a prop. When the Elcano-email path
// isn't configured — e.g. a white-labelled deploy — the secondary button and
// its divider are omitted entirely so the card shows only the password form
// and never surfaces the Elcano brand.
//
// title/tagline arrive the same way: as props resolved server-side from the
// bundle's `branding.login_title` / `login_tagline` (#892). They used to be
// hardcoded literals here, because this is a client component and
// /client-config — where those strings are served — is member-gated, so a
// pre-auth card structurally cannot fetch them. The strings were parsed,
// defaulted, API-served and typed, and then never rendered: a bundle setting
// login_title to "Reklaim what's yours." still displayed "Welcome aboard."
// The page-level server component reads them instead and hands them down.
export default function LoginCard({
  magicLinkLoginEnabled,
  oidcEnabled = false,
  oidcLabel = "Sign in with SSO",
  title,
  tagline,
}: {
  magicLinkLoginEnabled: boolean;
  oidcEnabled?: boolean;
  oidcLabel?: string;
  title: string;
  tagline: string;
}) {
  const [loginError, setLoginError] = useState<string | null>(null);

  // Reading the `?e=` query param must happen after hydration — `window` is
  // undefined during SSR, and a useState lazy initializer would cause a hydrate
  // mismatch for the initial render. We read synchronously in the effect but
  // apply the result via a microtask so the setState lands outside the effect's
  // synchronous phase (otherwise react-hooks/set-state-in-effect flags the
  // cascading render); a guard cancels the update if we unmount first. The
  // theme is owned by the shared ThemeToggle (useTheme) below.
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const nextError = errorCodeToMessage(params.get("e"));
    let cancelled = false;
    queueMicrotask(() => {
      if (cancelled) return;
      setLoginError(nextError);
    });
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <main className="flex min-h-screen items-center justify-center bg-[var(--gradient-bg-home-signature)] px-6 py-10">
      <div className="w-full max-w-sm rounded-[1.5rem] border border-[var(--color-border)] bg-[var(--composer-surface)] p-6 shadow-[var(--composer-shadow)]">
        <div className="mb-6 flex items-start justify-between gap-4">
          <div className="grid gap-2">
            <h1 className="text-[1.25rem] font-semibold text-[var(--color-text-primary)]">{title}</h1>
            <p className="text-[0.875rem] text-[var(--color-text-secondary)]">{tagline}</p>
          </div>

          <ThemeToggle className="inline-flex size-9 shrink-0 items-center justify-center rounded-full border border-[var(--color-border)] text-[var(--color-text-muted)] transition hover:bg-[var(--color-overlay-soft)] hover:text-[var(--color-text-primary)]" />
        </div>

        {loginError ? (
          <div className="mb-4 rounded-xl border border-[var(--color-danger-strong)] bg-[color-mix(in_srgb,var(--color-danger-strong)_14%,transparent)] px-3 py-2 text-[0.8125rem] text-[var(--color-danger-soft)]">
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
            className="mt-2 rounded-xl bg-[var(--color-primary)] px-4 py-2.5 text-sm font-medium text-[var(--color-on-primary)] transition hover:opacity-90"
          >
            Sign in
          </button>
        </form>

        {magicLinkLoginEnabled || oidcEnabled ? (
          <>
            <div className="my-5 flex items-center gap-3 text-[0.6875rem] uppercase tracking-wide text-[var(--color-text-muted)]">
              <span className="h-px flex-1 bg-[var(--color-border)]" />
              or
              <span className="h-px flex-1 bg-[var(--color-border)]" />
            </div>

            {/* Secondary sign-in(s): hand off to an external identity flow. Kept
                visually subordinate to the primary password action above, per the
                flag design system's primary-semantics rule.

                Both targets are route HANDLERS (app/api/auth/oidc/start/route.ts,
                app/api/auth/elcano-login/route.ts), not pages: each answers with a
                303 to a third-party identity provider, and /oidc/start also has to
                SET the state/nonce/PKCE cookies on that response. A next/link
                soft-navigation would fetch an RSC payload that does not exist and
                would never perform the cross-origin document navigation the flow
                depends on, so a plain <a> is the correct element here — the full
                page load is the handoff. oxlint's port of no-html-link-for-pages
                flags every root-relative href without resolving it against the
                route tree (upstream @next/next resolves the pages dir and would
                not fire here), hence the two suppressions below. */}
            <div className="grid gap-3">
              {oidcEnabled ? (
                // eslint-disable-next-line @next/next/no-html-link-for-pages -- /api/auth/oidc/start is a route handler that 303s to the IdP and sets PKCE cookies, not a Next page.
                <a
                  href="/api/auth/oidc/start"
                  className="flex items-center justify-center rounded-xl border border-[var(--color-border)] px-4 py-2.5 text-sm font-medium text-[var(--color-text-primary)] transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]"
                >
                  {oidcLabel}
                </a>
              ) : null}
              {magicLinkLoginEnabled ? (
                // eslint-disable-next-line @next/next/no-html-link-for-pages -- /api/auth/elcano-login is a route handler that 303s to the auth service, not a Next page.
                <a
                  href="/api/auth/elcano-login"
                  className="flex items-center justify-center rounded-xl border border-[var(--color-border)] px-4 py-2.5 text-sm font-medium text-[var(--color-text-primary)] transition hover:bg-[var(--color-overlay-soft)] focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]"
                >
                  Use Elcano email
                </a>
              ) : null}
            </div>
          </>
        ) : null}
      </div>
    </main>
  );
}
