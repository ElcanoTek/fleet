import { NextRequest, NextResponse } from "next/server";
import {
  getElcanoCookieDomain,
  getElcanoCookieName,
  getRedirectUrl,
  getSessionCookieName,
  isSecureRequest,
} from "@/app/lib/auth";
import { verifyOrigin } from "@/app/lib/csrf";
import {
  getOidcConfig,
  OIDC_NONCE_COOKIE,
  OIDC_STATE_COOKIE,
  OIDC_VERIFIER_COOKIE,
} from "@/app/lib/oidc";

/**
 * POST /api/auth/logout
 *
 * Clears BOTH session cookies and returns the user to chat's own /login page:
 *   - elcano_session — chat's HMAC password cookie (host-only).
 *   - elcano_auth     — the shared Ed25519 cookie minted by the auth service.
 *
 * We clear elcano_auth here (rather than bouncing through auth/logout) for two
 * reasons: the user should land back on chat's login, not auth's; and if we
 * left elcano_auth in place, an Elcano-email user would be logged straight back
 * in by the middleware and never see /login. chat can delete it because the
 * cookie lives on the shared parent domain (AUTH_COOKIE_DOMAIN) that chat's
 * host belongs to — and deleting the shared cookie signs the user out of the
 * other Elcano services too, which is the expected meaning of "log out".
 *
 * When central Auth is configured, the browser is then sent to the provider's
 * RP-initiated logout (`<issuer>/logout?client_id=<ours>`) whatever kind of
 * Fleet session it held — OIDC, password, or none at all. Fleet cannot see
 * Auth's host-only cookie, so it cannot know whether this browser also holds
 * a central session; a person who signed in to Fleet with the break-glass
 * password AND separately at Auth expects one Sign out to end both. Only a
 * top-level navigation can present that cookie to Auth, which ends every
 * central session of the account (all devices, the owner's policy), fans a
 * back-channel logout out to every application, clears its cookies and lands
 * on its signed-out page; with no central session it lands on the same page
 * with nothing to end. A deployment without central Auth (or with a malformed
 * issuer) lands on /login?manual=1: the card with both options and no silent
 * SSO attempt. The in-progress OIDC transaction cookies are cleared too, so a
 * callback still in flight cannot mint a fresh session after the user asked
 * to leave.
 *
 * The reverse direction is narrower on purpose: Auth's back-channel logout
 * rotates the external epoch and so ends Fleet sessions that came from Auth,
 * but leaves Fleet's own password sessions alone (break-glass isolation).
 */
export async function POST(request: NextRequest) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const oidc = getOidcConfig();
  let landing: URL | string = getRedirectUrl(request, "/login?manual=1");
  if (oidc) {
    // getOidcConfig only checks the issuer is non-empty; a malformed value
    // must not turn logout into a 500 that leaves every cookie in place.
    try {
      const endSession = new URL("/logout", oidc.issuer);
      endSession.searchParams.set("client_id", oidc.clientId);
      landing = endSession.toString();
    } catch {
      landing = getRedirectUrl(request, "/login?manual=1");
    }
  }

  const secure = isSecureRequest(request);
  const res = NextResponse.redirect(landing, { status: 303 });
  res.headers.set("Cache-Control", "no-store");

  const attrs = `Path=/; Max-Age=0; HttpOnly; SameSite=Lax${secure ? "; Secure" : ""}`;
  res.headers.append("Set-Cookie", `${getSessionCookieName()}=; ${attrs}`);

  // Cookie deletion matches on name + domain + path, so mirror how auth set
  // it. But AUTH_COOKIE_DOMAIN is config that can drift from how the auth
  // service actually minted the cookie (a fresh deployment with the env unset
  // deletes host-only while the live cookie carries Domain=…), and a deletion
  // that misses its shape silently no-ops: the user sees the login page, then
  // the next load is signed back in. Deleting a shape that doesn't exist is
  // harmless, so send BOTH variants — appended as raw headers because a
  // cookie store keyed by name would collapse them into one.
  const elcanoDomain = getElcanoCookieDomain();
  if (elcanoDomain) {
    res.headers.append(
      "Set-Cookie",
      `${getElcanoCookieName()}=; Domain=${elcanoDomain}; ${attrs}`,
    );
  }
  res.headers.append("Set-Cookie", `${getElcanoCookieName()}=; ${attrs}`);
  for (const name of [
    OIDC_STATE_COOKIE,
    OIDC_NONCE_COOKIE,
    OIDC_VERIFIER_COOKIE,
  ]) {
    res.headers.append("Set-Cookie", `${name}=; ${attrs}`);
  }

  return res;
}
