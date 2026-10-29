import { getBootstrap } from "@/app/lib/bootstrap";
import LoginCard from "./login-card";

// /login. What used to be resolved by a server component at request time now
// arrives in the server-rendered bootstrap (internal/webui/spa.go): whether the
// "Use Elcano email" path is configured (AUTH_SIGNING_PUBKEY), the SSO button
// and its label (FLEET_OIDC_*), the bundle's login copy (/brand/meta), and
// whether the visitor is signing in to open a team link (B24), and whether
// Fleet passwords are on at all (off when central Auth is configured, unless
// FLEET_OIDC_ALLOW_PASSWORD). The silent SSO auto-start
// (FLEET_OIDC_AUTO_START) is a server redirect before this page.
export default function LoginPage() {
  const { login, branding } = getBootstrap();
  return (
    <LoginCard
      teamLink={login.teamLink}
      passwordLoginEnabled={login.passwordLoginEnabled}
      magicLinkLoginEnabled={login.magicLinkLoginEnabled}
      oidcEnabled={login.oidcEnabled}
      oidcLabel={login.oidcLabel}
      title={branding.loginTitle}
      tagline={branding.loginTagline}
      appName={branding.appName}
    />
  );
}
