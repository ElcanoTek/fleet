"use client";

// Orchestrator sign-in card. Cookie/OIDC is the only operator path: the
// leftover moc username/password form is gone (EnsureAdminUser mints an
// unusable random password, so that form could not admit a real operator).
// Backend POST /auth/login + the bearer proxy stay for API clients.
//
// A genuinely signed-out visitor almost never sees this card: the Next gate
// bounces them to /login first. This is the safety-net for a 401 /me after
// that gate, plus the "Use Elcano email" handoff when magic-link is enabled.

import Link from "next/link";

export type OrchestratorLoginProps = {
  magicLinkLoginEnabled: boolean;
};

export function OrchestratorLogin({ magicLinkLoginEnabled }: OrchestratorLoginProps) {
  return (
    <div className="auth-section" role="region" aria-label="Authentication">
      <div className="auth-fields stack-form">
        <h2>Sign in</h2>
        <p className="caption">
          Sign in with the same account you use for Chat. Operations Center
          access is provisioned by an administrator.
        </p>

        {magicLinkLoginEnabled ? (
          // A plain <a> is required here, not next/link: the target is a route
          // HANDLER (app/api/orchestrator/auth/elcano-login/route.ts), not a
          // page. It answers with a 303 to the auth service's magic-link login
          // on another origin, so there is no RSC payload for the client router
          // to render — a <Link> soft-navigation would break the handoff. The
          // oxlint port of this rule flags every root-relative href without
          // resolving it against the route tree (upstream @next/next resolves
          // the pages dir and would not fire here), hence the suppression.
          // eslint-disable-next-line @next/next/no-html-link-for-pages -- /api/* is a route handler that 303s to an external IdP, not a Next page.
          <a
            className="btn btn-primary"
            href="/api/orchestrator/auth/elcano-login"
            aria-label="Sign in with your Elcano email"
          >
            Use Elcano email
          </a>
        ) : (
          // /login IS a real page (app/login/page.tsx), so this is genuine
          // in-app navigation and belongs to the client router. prefetch is off
          // deliberately: the proxy gate (src/proxy.ts) bounces a request for
          // /login that already carries a session to /chat, and this card can
          // render while a chat-side cookie exists (the orchestrator /me 401s
          // independently) — speculatively fetching it would only cache that
          // redirect. Clicking still follows whatever the gate decides, exactly
          // as the old full-page <a> did.
          <Link className="btn btn-primary" href="/login" prefetch={false}>
            Sign in via Chat
          </Link>
        )}
      </div>
    </div>
  );
}

export default OrchestratorLogin;
