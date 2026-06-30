import { NextRequest, NextResponse } from "next/server";
import { getRedirectUrl, isSecureRequest } from "@/app/lib/auth";
import {
  buildRedirectUri,
  discover,
  getOidcConfig,
  OIDC_NONCE_COOKIE,
  OIDC_STATE_COOKIE,
  OIDC_VERIFIER_COOKIE,
  oidcTempCookieMaxAgeSeconds,
  pkceChallenge,
  randomUrlSafe,
} from "@/app/lib/oidc";

export const runtime = "nodejs";

/**
 * GET /api/auth/oidc/start
 *
 * Target of the SSO button on the login card (#240). Begins the Authorization
 * Code + PKCE flow: mint state + nonce + a PKCE verifier, stash them in
 * short-lived httpOnly cookies, and 303 the browser to the IdP's authorization
 * endpoint. The IdP returns the browser to /api/auth/oidc/callback.
 *
 * Disabled (no FLEET_OIDC_* config) or a broken discovery doc bounces back to
 * the password login with an error rather than trapping the user.
 */
export async function GET(request: NextRequest): Promise<NextResponse> {
  const config = getOidcConfig();
  if (!config) {
    return NextResponse.redirect(getRedirectUrl(request, "/login?e=oidc_unavailable"), { status: 303 });
  }

  let authorizationEndpoint: string;
  try {
    const disco = await discover(config.issuer);
    authorizationEndpoint = disco.authorization_endpoint;
  } catch {
    return NextResponse.redirect(getRedirectUrl(request, "/login?e=oidc_error"), { status: 303 });
  }

  const state = randomUrlSafe();
  const nonce = randomUrlSafe();
  const verifier = randomUrlSafe(64);
  const challenge = await pkceChallenge(verifier);
  const redirectUri = buildRedirectUri(config, request);

  const authUrl = new URL(authorizationEndpoint);
  authUrl.searchParams.set("response_type", "code");
  authUrl.searchParams.set("client_id", config.clientId);
  authUrl.searchParams.set("redirect_uri", redirectUri);
  authUrl.searchParams.set("scope", config.scopes);
  authUrl.searchParams.set("state", state);
  authUrl.searchParams.set("nonce", nonce);
  authUrl.searchParams.set("code_challenge", challenge);
  authUrl.searchParams.set("code_challenge_method", "S256");

  const res = NextResponse.redirect(authUrl.toString(), { status: 303 });
  const secure = isSecureRequest(request);
  for (const [name, value] of [
    [OIDC_STATE_COOKIE, state],
    [OIDC_NONCE_COOKIE, nonce],
    [OIDC_VERIFIER_COOKIE, verifier],
  ] as const) {
    res.cookies.set({
      name,
      value,
      httpOnly: true,
      sameSite: "lax",
      secure,
      maxAge: oidcTempCookieMaxAgeSeconds,
      path: "/",
    });
  }
  return res;
}
