import { getAuthSigningPubkey } from "@/app/lib/auth";
import { getOidcConfig } from "@/app/lib/oidc";
import { getServerBranding } from "@/app/lib/serverBranding";
import LoginCard from "./login-card";

// force-dynamic is load-bearing: the deploy build (scripts/update.sh) runs in a
// staging dir with .env.local excluded, so AUTH_SIGNING_PUBKEY is unset at
// `next build` time. Without this, /login statically prerenders with the button
// baked OFF for every deploy — including Elcano's own. Rendering per request
// reads the live runtime env (`next start` runs in $APP_DIR with .env.local).
export const dynamic = "force-dynamic";

// The login route is a server component so it can read AUTH_SIGNING_PUBKEY at
// request time — the same gate the Elcano-email backend uses — and pass it to
// the (client) card. Resolving it at runtime (rather than a NEXT_PUBLIC_*
// build-time flag) keeps the toggle a pure ops switch: the "Use Elcano email"
// button appears only where the magic-link path is actually configured, so a
// white-labelled deploy that never sets the key shows only the password form.
export default async function LoginPage() {
  // OIDC (#240) is resolved server-side at request time too (same rationale as
  // the Elcano-email flag above): the SSO button appears only where FLEET_OIDC_*
  // is actually configured, with the operator-chosen label.
  const oidc = getOidcConfig();
  // The bundle's login copy (#892). This server component is the right place for
  // it: it already resolves runtime state the client card cannot see, and
  // /brand/meta is token-gated + identity-less precisely so the pre-auth shell
  // can read branding. getServerBranding never throws — a backend that is down
  // yields fleet's defaults rather than failing the one page a locked-out
  // operator needs to reach.
  const branding = await getServerBranding();
  return (
    <LoginCard
      magicLinkLoginEnabled={getAuthSigningPubkey() !== ""}
      oidcEnabled={oidc !== null}
      oidcLabel={oidc?.buttonLabel ?? "Sign in with SSO"}
      title={branding.loginTitle}
      tagline={branding.loginTagline}
    />
  );
}
