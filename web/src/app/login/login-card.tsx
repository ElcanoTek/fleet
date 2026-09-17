"use client";

import Image from "next/image";
import { useEffect, useState } from "react";
import { ThemeToggle } from "@/app/shared/ui/ThemeToggle";

// errorCodeToMessage maps the `?e=` query param our login handler redirects
// with to a human-readable message. We keep "invalid" deliberately vague
// so the UI can't be used to enumerate which email addresses exist.
function errorCodeToMessage(code: string | null): string | null {
  if (!code) return null;
  if (code === "invalid") return "Invalid email or password.";
  if (code === "missing") return "Please enter both email and password.";
  if (code === "server")
    return "The chat server isn't reachable right now. Try again in a moment.";
  // "in a minute" matches the verify endpoint's Retry-After: 60
  // (internal/httpapi/auth_verify.go).
  if (code === "throttled")
    return "Too many sign-in attempts. Try again in a minute.";
  if (code === "elcano_unavailable")
    return "Elcano email sign-in isn't available right now. Use your email and password.";
  if (code === "oidc_unavailable")
    return "Single sign-on isn't available right now. Use your email and password.";
  if (code === "oidc_denied") return "Single sign-on was cancelled.";
  if (code === "oidc_domain")
    return "Your account's email domain isn't allowed to sign in here.";
  if (code === "oidc_error")
    return "Single sign-on failed. Try again, or use your email and password.";
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
  appName,
}: {
  magicLinkLoginEnabled: boolean;
  oidcEnabled?: boolean;
  oidcLabel?: string;
  title: string;
  tagline: string;
  /** The bundle's app_name, rendered as the small uppercase wordmark above the title. */
  appName: string;
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

  const secondary =
    "flex min-h-11 items-center justify-center rounded-[var(--radius-md)] border border-[var(--color-border)] bg-[var(--color-surface-1)] px-4 py-2.5 text-sm font-bold text-[var(--color-text-secondary)] transition hover:border-[var(--color-border-strong)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]";
  const field =
    "block min-h-10 w-full rounded-[var(--radius-md)] border border-[var(--color-border-strong)] bg-[var(--color-surface-1)] px-3 py-2 text-[var(--color-text-primary)] outline-none transition hover:border-[var(--color-primary)] focus-visible:border-[var(--color-primary)] focus-visible:[box-shadow:var(--focus-ring)]";
  const label =
    "mb-2 block text-[0.8125rem] font-bold text-[var(--color-text-secondary)]";

  // The primary action uses Auth's button gradient (primary → primary_hover)
  // rather than Fleet's app-wide --gradient-action-primary (primary →
  // secondary), so the two sign-in buttons are identical; the app's own
  // buttons are untouched.
  // The card is Auth's sign-in card, term for term (auth/internal/httpapi/
  // templates.go: loginHTML + componentCSS): the bundle mark, the wordmark,
  // the title and tagline, labelled fields on surface_1, one full-width
  // primary action, then the secondary sign-in(s) under an "or" divider. Same
  // tokens, same gradients, so a person moving between Auth and Fleet sees
  // one design. The theme toggle sits at the page corner as it does on Auth.
  return (
    <main className="flex min-h-screen bg-[var(--gradient-bg-home-signature)] px-6 py-10">
      <ThemeToggle className="fixed top-5 right-5 inline-flex size-10 items-center justify-center rounded-full border border-[var(--color-border)] bg-[var(--color-surface-1)] text-[var(--color-text-secondary)] transition hover:border-[var(--color-border-strong)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]" />
      <div className="m-auto w-full max-w-[25rem] rounded-[var(--radius-xl)] border border-[var(--color-border)] bg-[var(--gradient-surface-card)] p-10 shadow-[var(--shadow-lg)]">
        {/* unoptimized for the same reason as the rail's mark (NavRail.tsx): the
            bundle mark is served at /api/brand/logo with no extension, and an
            SVG through next/image's optimizer renders broken. */}
        <Image
          src="/api/brand/logo"
          alt=""
          width={36}
          height={36}
          priority
          unoptimized
          className="mb-4 block h-9 w-auto max-w-[12rem]"
        />
        <div className="mb-5 text-[0.75rem] font-bold tracking-[0.14em] text-[var(--color-text-muted)] uppercase">
          {appName}
        </div>
        <h1 className="font-heading mb-2 text-[1.75rem] leading-[1.2] font-bold tracking-[-0.01em] text-[var(--color-text-primary)]">
          {title}
        </h1>
        <p className="mb-6 text-[var(--color-text-muted)]">{tagline}</p>

        {loginError ? (
          <div
            role="alert"
            className="mb-5 rounded-[var(--radius-md)] border border-[var(--color-danger-border)] bg-[color-mix(in_srgb,var(--color-danger)_14%,transparent)] px-3 py-3 text-[0.8125rem] text-[var(--color-danger)]"
          >
            {loginError}
          </div>
        ) : null}

        <form action="/api/auth/login" method="post">
          <label htmlFor="email" className={label}>
            Email
          </label>
          <input
            id="email"
            name="email"
            type="email"
            autoComplete="email"
            required
            placeholder="you@example.com"
            className={`${field} mb-4`}
          />
          <label htmlFor="password" className={label}>
            Password
          </label>
          <input
            id="password"
            name="password"
            type="password"
            autoComplete="current-password"
            required
            className={field}
          />
          <button
            type="submit"
            className="mt-5 inline-flex min-h-11 w-full items-center justify-center rounded-[var(--radius-md)] bg-[linear-gradient(140deg,var(--color-primary),var(--color-primary-hover))] px-4 py-3 font-bold text-[var(--color-on-primary)] transition hover:brightness-[1.08] focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)] active:translate-y-px"
          >
            Sign in
          </button>
        </form>

        {magicLinkLoginEnabled || oidcEnabled ? (
          <>
            <div className="my-5 flex items-center gap-3 text-[0.6875rem] tracking-wide text-[var(--color-text-muted)] uppercase">
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
                <a href="/api/auth/oidc/start" className={secondary}>
                  {oidcLabel}
                </a>
              ) : null}
              {magicLinkLoginEnabled ? (
                // eslint-disable-next-line @next/next/no-html-link-for-pages -- /api/auth/elcano-login is a route handler that 303s to the auth service, not a Next page.
                <a href="/api/auth/elcano-login" className={secondary}>
                  Use Elcano email
                </a>
              ) : null}
            </div>
          </>
        ) : null}

        <p className="mt-6 text-center text-[0.8125rem] text-[var(--color-text-muted)]">
          Accounts are created by an administrator.
        </p>
      </div>
    </main>
  );
}
