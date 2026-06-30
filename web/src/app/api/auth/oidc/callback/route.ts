import { NextRequest, NextResponse } from "next/server";
import {
  createSessionToken,
  getRedirectUrl,
  getSessionCookieName,
  isSecureRequest,
  sessionMaxAgeSeconds,
} from "@/app/lib/auth";
import {
  buildRedirectUri,
  decodeJwtClaims,
  discover,
  emailDomainAllowed,
  getOidcConfig,
  OIDC_NONCE_COOKIE,
  OIDC_STATE_COOKIE,
  OIDC_VERIFIER_COOKIE,
  validateIdToken,
} from "@/app/lib/oidc";

export const runtime = "nodejs";

/**
 * GET /api/auth/oidc/callback
 *
 * Completes the Authorization Code + PKCE flow (#240):
 *   1. Validate the `state` against the cookie planted by /start (CSRF).
 *   2. Exchange `code` (+ PKCE verifier + client_secret) at the token endpoint.
 *   3. Validate the ID token's claims (iss/aud/exp/nonce) — signature trust
 *      comes from the server-to-server TLS channel (see lib/oidc header).
 *   4. Enforce the optional email-domain allowlist.
 *   5. Mint the standard elcano_session cookie (same one the password form
 *      mints) so every downstream gate works unchanged, and 303 home.
 *
 * Membership is still enforced by the chat server's user-list (a successful SSO
 * login for an un-provisioned email lands on the no-access page, exactly as an
 * elcano_auth login does). Any failure clears the temp cookies and bounces to
 * /login with a coarse, non-enumerating error code.
 */
export async function GET(request: NextRequest): Promise<NextResponse> {
  const config = getOidcConfig();
  if (!config) {
    return NextResponse.redirect(getRedirectUrl(request, "/login?e=oidc_unavailable"), { status: 303 });
  }

  const fail = (code: string) => {
    const res = NextResponse.redirect(getRedirectUrl(request, `/login?e=${code}`), { status: 303 });
    clearTempCookies(res, isSecureRequest(request));
    return res;
  };

  const params = request.nextUrl.searchParams;
  if (params.get("error")) {
    // The user declined consent, or the IdP rejected the request.
    return fail("oidc_denied");
  }

  const code = params.get("code");
  const state = params.get("state");
  const cookieState = request.cookies.get(OIDC_STATE_COOKIE)?.value;
  const nonce = request.cookies.get(OIDC_NONCE_COOKIE)?.value;
  const verifier = request.cookies.get(OIDC_VERIFIER_COOKIE)?.value;

  if (!code || !state || !cookieState || !nonce || !verifier) return fail("oidc_error");
  // Constant work is unnecessary here (state is our own random value, not a
  // secret tied to a user); a direct compare is the CSRF gate.
  if (state !== cookieState) return fail("oidc_error");

  let tokenEndpoint: string;
  let issuerDoc;
  try {
    issuerDoc = await discover(config.issuer);
    tokenEndpoint = issuerDoc.token_endpoint;
  } catch {
    return fail("oidc_error");
  }

  // Token exchange — confidential client (client_secret) + PKCE verifier.
  let idToken: string;
  try {
    const body = new URLSearchParams({
      grant_type: "authorization_code",
      code,
      redirect_uri: buildRedirectUri(config, request),
      client_id: config.clientId,
      client_secret: config.clientSecret,
      code_verifier: verifier,
    });
    const tokenRes = await fetch(tokenEndpoint, {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded", Accept: "application/json" },
      body,
    });
    if (!tokenRes.ok) return fail("oidc_error");
    const tokens = (await tokenRes.json()) as { id_token?: string };
    if (!tokens.id_token) return fail("oidc_error");
    idToken = tokens.id_token;
  } catch {
    return fail("oidc_error");
  }

  const validation = validateIdToken(decodeJwtClaims(idToken), config, issuerDoc, nonce);
  if (!validation.ok) return fail("oidc_error");

  if (!emailDomainAllowed(validation.email, config.allowedDomains)) {
    return fail("oidc_domain");
  }

  // Authenticated — mint the standard session cookie and go home.
  const res = NextResponse.redirect(getRedirectUrl(request, "/"), { status: 303 });
  const secure = isSecureRequest(request);
  res.cookies.set({
    name: getSessionCookieName(),
    value: await createSessionToken(validation.email),
    httpOnly: true,
    sameSite: "lax",
    secure,
    maxAge: sessionMaxAgeSeconds,
    path: "/",
  });
  clearTempCookies(res, secure);
  return res;
}

function clearTempCookies(res: NextResponse, secure: boolean) {
  for (const name of [OIDC_STATE_COOKIE, OIDC_NONCE_COOKIE, OIDC_VERIFIER_COOKIE]) {
    res.cookies.set({ name, value: "", httpOnly: true, sameSite: "lax", secure, maxAge: 0, path: "/" });
  }
}
