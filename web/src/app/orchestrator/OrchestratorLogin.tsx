import { t as tr } from "@/app/lib/i18n";
// Orchestrator sign-in card. Cookie/OIDC is the only operator path: the
// leftover moc username/password form is gone, and so is the backend's
// POST /auth/login and its bearer (#2049 M4): programs use API keys.
//
// A genuinely signed-out visitor almost never sees this card: the Next gate
// bounces them to /login first. This is the safety-net for a 401 /me after
// that gate, plus the "Use Elcano email" handoff when magic-link is enabled.

import { withReturnTo } from "@/app/chat/ui/bootstrapFailure";
import { Link } from "@/app/lib/Link";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";

export type OrchestratorLoginProps = {
  magicLinkLoginEnabled: boolean;
};

export function OrchestratorLogin({ magicLinkLoginEnabled }: OrchestratorLoginProps) {
  // The page the card is on — a task link's ?task= while it is still waiting
  // to open, a tab — goes through sign-in with the visitor, so they come back
  // to it rather than to the start page (internal/webui/returnto.go).
  const returnTo =
    typeof window === "undefined" ? null : window.location.pathname + window.location.search;
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
              href={withReturnTo("/api/orchestrator/auth/elcano-login", returnTo)}
              aria-label={tr("Sign in with your Elcano email")}
            >
              {tr("Use Elcano email")}
            </a>
          </Button>
        ) : (
          // /login IS a real page (app/login/page.tsx), so this is genuine
          // in-app navigation and belongs to the client router.
          <Button asChild className="btn btn-primary">
            <Link href={withReturnTo("/login", returnTo)}>
              {tr("Sign in via Chat")}
            </Link>
          </Button>
        )}
      </div>
    </Card>
  );
}

export default OrchestratorLogin;
