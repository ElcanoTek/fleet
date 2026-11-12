import { getLanguage, t as tr } from "@/app/lib/i18n";
import { useEffect, useState } from "react";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { ThemeToggle } from "@/app/shared/ui/ThemeToggle";
import { LanguageSelect } from "@/app/shared/ui/LanguageSelect";

// errorCodeToMessage maps the `?e=` query param our login handler redirects
// with to a human-readable message. We keep "invalid" deliberately vague
// so the UI can't be used to enumerate which email addresses exist. Where a
// message would point at the password form, it says so only when the card
// has one.
function errorCodeToMessage(
  code: string | null,
  passwordLoginEnabled: boolean,
): string | null {
  if (!code) return null;
  if (!passwordLoginEnabled) {
    if (code === "password_off" || code === "invalid" || code === "missing")
      return tr("Password sign-in is turned off here. Sign in with SSO.");
    if (code === "elcano_unavailable")
      return tr("Elcano email sign-in isn't available right now. Sign in with SSO.");
    if (code === "oidc_unavailable")
      return tr("Single sign-on isn't available right now. Try again in a moment.");
    if (code === "oidc_error") return tr("Single sign-on failed. Try again.");
  }
  if (code === "invalid") return tr("Invalid email or password.");
  if (code === "missing") return tr("Please enter both email and password.");
  if (code === "server")
    return tr("The chat server isn't reachable right now. Try again in a moment.");
  // "in a minute" matches the verify endpoint's Retry-After: 60
  // (internal/httpapi/auth_verify.go).
  if (code === "throttled")
    return tr("Too many sign-in attempts. Try again in a minute.");
  if (code === "elcano_unavailable")
    return tr("Elcano email sign-in isn't available right now. Use your email and password.");
  if (code === "oidc_unavailable")
    return tr("Single sign-on isn't available right now. Use your email and password.");
  if (code === "oidc_denied") return tr("Single sign-on was cancelled.");
  if (code === "oidc_domain")
    return tr("Your account's email domain isn't allowed to sign in here.");
  if (code === "oidc_error")
    return tr("Single sign-on failed. Try again, or use your email and password.");
  return tr("Could not sign in.");
}

export default function LoginCard({
  passwordLoginEnabled = true,
  magicLinkLoginEnabled,
  oidcEnabled = false,
  oidcLabel = tr("Sign in with SSO"),
  title,
  tagline,
  appName,
  teamLink = false,
}: {
  passwordLoginEnabled?: boolean;
  magicLinkLoginEnabled: boolean;
  oidcEnabled?: boolean;
  oidcLabel?: string;
  title: string;
  tagline: string;
  /** The bundle's app_name, rendered as the small uppercase wordmark above the title. */
  appName: string;
  /**
   * The visitor opened a team link while signed out (B24): say why they are
   * signing in, and that they will land on the chat afterwards (the proxy
   * redirects there once a session exists). Replaces the bundle's welcome copy
   * for this one visit only.
   */
  teamLink?: boolean;
}) {
  const [loginError, setLoginError] = useState<string | null>(null);

  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    const nextError = errorCodeToMessage(params.get("e"), passwordLoginEnabled);
    let cancelled = false;
    queueMicrotask(() => {
      if (cancelled) return;
      setLoginError(nextError);
    });
    return () => {
      cancelled = true;
    };
  }, [passwordLoginEnabled]);

  const primaryClass =
    "min-h-11 w-full bg-[linear-gradient(140deg,var(--color-primary-fill),var(--color-primary-fill-hover))] px-4 py-3 font-bold text-[var(--color-on-primary-fill)] hover:bg-[linear-gradient(140deg,var(--color-primary-fill-hover),var(--color-primary-fill-hover))]";
  const secondaryClass =
    "min-h-11 w-full bg-[var(--color-surface-1)] px-4 py-2.5 text-sm font-bold text-[var(--color-text-secondary)] hover:text-[var(--color-text-primary)]";
  const fieldClass =
    "bg-[var(--color-surface-1)]";
  const labelClass =
    "mb-2 block text-[0.8125rem] font-bold text-[var(--color-text-secondary)]";

  return (
    <main className="flex min-h-screen bg-[var(--gradient-bg-home-signature)] px-6 py-10">
      <div className="fixed top-5 left-5"><LanguageSelect /></div>
      <ThemeToggle className="fixed top-5 right-5 inline-flex size-10 items-center justify-center rounded-full border border-[var(--color-border)] bg-[var(--color-surface-1)] text-[var(--color-text-secondary)] transition hover:border-[var(--color-border-strong)] hover:text-[var(--color-text-primary)] focus-visible:outline-none focus-visible:[box-shadow:var(--focus-ring)]" />
      <Card className="m-auto block w-full max-w-[25rem] gap-0 rounded-[var(--radius-xl)] border-[var(--color-border)] bg-[var(--gradient-surface-card)] p-10 shadow-[var(--shadow-lg)]">
        {/* A plain <img>: the bundle mark is served at /api/brand/logo with no
            extension and may be an SVG, which no image optimizer should touch. */}
        <img
          src="/api/brand/logo"
          alt=""
          width={36}
          height={36}
          fetchPriority="high"
          decoding="async"
          style={{ color: "transparent" }}
          className="mb-4 block h-9 w-auto max-w-[12rem]"
        />
        <div className="mb-5 text-[0.75rem] font-bold tracking-[0.14em] text-[var(--color-text-muted)] uppercase">
          {appName}
        </div>
        <h1 className="font-heading mb-2 text-[1.75rem] leading-[1.2] font-bold tracking-[-0.01em] text-[var(--color-text-primary)]">
          {teamLink ? tr("Sign in to open this chat") : title}
        </h1>
        <p className="mb-6 text-[1rem] leading-[1.5] text-[var(--color-text-muted)]">
          {teamLink
            ? tr("You’ll go straight to the chat after you sign in.")
            : tagline}
        </p>

        {loginError ? (
          <Alert variant="destructive" className="mb-5 block px-3 py-3 text-[0.8125rem]">
            <AlertDescription className="block text-[0.8125rem] text-inherit">
              {loginError}
            </AlertDescription>
          </Alert>
        ) : null}

        {passwordLoginEnabled ? (
          <form action="/api/auth/login" method="post">
            <Label htmlFor="email" className={labelClass}>
              {tr("Email")}
            </Label>
            <Input
              id="email"
              name="email"
              type="email"
              autoComplete="email"
              required
              placeholder={tr("you@example.com")}
              className={`${fieldClass} mb-4`}
            />
            <Label htmlFor="password" className={labelClass}>
              {tr("Password")}
            </Label>
            <Input
              id="password"
              name="password"
              type="password"
              autoComplete="current-password"
              required
              className={fieldClass}
            />
            <Button type="submit" size="lg" className={`mt-5 ${primaryClass}`}>
              {tr("Sign in")}
            </Button>
          </form>
        ) : (
          <div className="grid gap-3">
            {oidcEnabled ? (
              <Button asChild size="lg" className={primaryClass}>
                <a href={`/api/auth/oidc/start?lang=${getLanguage()}`}>
                  {oidcLabel}
                </a>
              </Button>
            ) : null}
            {magicLinkLoginEnabled ? (
              <Button asChild variant="outline" size="lg" className={secondaryClass}>
                <a href={`/api/auth/elcano-login?lang=${getLanguage()}`}>
                  {tr("Use Elcano email")}
                </a>
              </Button>
            ) : null}
          </div>
        )}

        {passwordLoginEnabled && (magicLinkLoginEnabled || oidcEnabled) ? (
          <>
            <div className="my-5 flex items-center gap-3 text-[0.6875rem] tracking-wide text-[var(--color-text-muted)] uppercase">
              <span className="h-px flex-1 bg-[var(--color-border)]" />
              {tr("or")}
              <span className="h-px flex-1 bg-[var(--color-border)]" />
            </div>

            <div className="grid gap-3">
              {oidcEnabled ? (
                <Button asChild variant="outline" size="lg" className={secondaryClass}>
                  <a href={`/api/auth/oidc/start?lang=${getLanguage()}`}>
                    {oidcLabel}
                  </a>
                </Button>
              ) : null}
              {magicLinkLoginEnabled ? (
                <Button asChild variant="outline" size="lg" className={secondaryClass}>
                  <a href={`/api/auth/elcano-login?lang=${getLanguage()}`}>
                    {tr("Use Elcano email")}
                  </a>
                </Button>
              ) : null}
            </div>
          </>
        ) : null}

        <p className="mt-6 text-center text-[0.8125rem] text-[var(--color-text-muted)]">
          {tr("Accounts are created by an administrator.")}
        </p>
      </Card>
    </main>
  );
}
