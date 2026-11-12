import { t as tr } from "@/app/lib/i18n";
// Orchestrator sign-in card. Cookie/OIDC is the only operator path: the
// leftover moc username/password form is gone, and so is the backend's
// POST /auth/login and its bearer (#2049 M4): programs use API keys.
//
// A genuinely signed-out visitor almost never sees this card: the Next gate
// bounces them to /login first. This is the safety-net for a 401 /me after
// that gate, plus the "Use Elcano email" handoff when magic-link is enabled.

import { Link } from "@/app/lib/Link";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";

export type OrchestratorLoginProps = {
  magicLinkLoginEnabled: boolean;
};

export function OrchestratorLogin({ magicLinkLoginEnabled }: OrchestratorLoginProps) {
  return (
    <Card className="auth-section" role="region" aria-label={tr("Authentication")}>
      <div className="auth-fields stack-form">
        <h2>{tr("Sign in")}</h2>
        <p className="caption">
          {tr("Sign in with the same account you use for Chat. Access to background tasks is provisioned by an administrator.")}
        </p>

        {magicLinkLoginEnabled ? (
          // A plain <a> is required here, not <Link>: the target is an API
          // route (/api/orchestrator/auth/elcano-login), not a page. It answers
          // with a 303 to the auth service's magic-link login on another
          // origin, so the full document navigation is the handoff — a <Link>
          // soft-navigation would never perform it.
          <Button asChild className="btn btn-primary">
            <a
              href="/api/orchestrator/auth/elcano-login"
              aria-label={tr("Sign in with your Elcano email")}
            >
              {tr("Use Elcano email")}
            </a>
          </Button>
        ) : (
          // /login IS a real page (app/login/page.tsx), so this is genuine
          // in-app navigation and belongs to the client router.
          <Button asChild className="btn btn-primary">
            <Link href="/login">
              {tr("Sign in via Chat")}
            </Link>
          </Button>
        )}
      </div>
    </Card>
  );
}

export default OrchestratorLogin;
