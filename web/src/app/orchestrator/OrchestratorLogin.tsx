"use client";

// Orchestrator sign-in card. Cookie/OIDC is the only operator path: the
// leftover moc username/password form is gone (EnsureAdminUser mints an
// unusable random password, so that form could not admit a real operator).
// Backend POST /auth/login + the bearer proxy stay for API clients.
//
// A genuinely signed-out visitor almost never sees this card: the Next gate
// bounces them to /login first. This is the safety-net for a 401 /me after
// that gate, plus the "Use Elcano email" handoff when magic-link is enabled.

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
          <a
            className="btn btn-primary"
            href="/api/orchestrator/auth/elcano-login"
            aria-label="Sign in with your Elcano email"
          >
            Use Elcano email
          </a>
        ) : (
          <a className="btn btn-primary" href="/login">
            Sign in via Chat
          </a>
        )}
      </div>
    </div>
  );
}

export default OrchestratorLogin;
