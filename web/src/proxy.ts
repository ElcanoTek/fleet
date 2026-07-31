import { NextResponse } from "next/server";
import type { NextRequest } from "next/server";
import { getRedirectUrl, getSessionFromRequest } from "@/app/lib/auth";
import { BUILD_ID_HEADER, currentBuildId } from "@/app/lib/buildId";

// ONE gate for the unified frontend. It protects BOTH views — /chat/* and
// /orchestrator/* — behind the same session check, and accepts BOTH login
// paths:
//   - elcano_auth: the Ed25519 cookie minted by the auth service
//     ("Use Elcano email").
//   - elcano_session: the HMAC cookie minted by the password form.
//   - moc bearer: when a request carries an Authorization: Bearer <token>
//     header (moc's username/password login persists a bearer token), the
//     orchestrator API proxy forwards it upstream, so this request proxy lets the
//     request through to be authorized by the orchestrator. The bearer is
//     opaque to the Next layer (moc owns it), so the only sane gate here is
//     "a bearer is present" — the real check happens at :8000.
// Both resolved by getSessionFromRequest (cookies) or detected as a bearer.

const publicPaths = new Set(["/login"]);
// Public API routes reachable without a session:
//   - elcano-login bounces the (unauthenticated) browser to the auth service.
//   - login / logout are the password form's targets.
//   - orchestrator/auth/login is moc's username/password login — it must be
//     reachable to obtain the bearer in the first place.
const publicApiPaths = new Set([
  "/api/auth/login",
  "/api/auth/logout",
  "/api/auth/elcano-login",
  // OIDC SSO (#240): both legs are pre-session by definition — /start bounces an
  // unauthenticated browser to the IdP, /callback receives the IdP's redirect
  // and mints the session. They must bypass the gate or the user can never reach
  // the IdP (the start of every SSO login).
  "/api/auth/oidc/start",
  "/api/auth/oidc/callback",
  "/api/orchestrator/auth/login",
  "/api/orchestrator/auth/logout",
  "/api/orchestrator/auth/elcano-login",
  // The brand-theme stylesheet is pre-session by design: layout.tsx links it on
  // every page so the LOGIN page paints in the bundle's palette before any
  // session exists. The route is deployment-wide, non-secret, and fail-safe
  // (always 200; empty CSS on backend trouble) — see app/api/theme/route.ts.
  // Without this entry the gate 401s it and the login page renders unthemed
  // (#903).
  "/api/theme",
]);

// contentSecurityPolicy builds the CSP for a response (#590). Two tiers:
//
//   - Baseline (every route): 'self' everywhere, plus only what the app
//     actually needs — Next.js injects inline scripts (hydration/RSC payload)
//     and inline styles, workspace/file previews use data:/blob: URLs, and
//     assistant markdown + the email preview may legitimately show public
//     https images. Dev mode additionally needs 'unsafe-eval' (react-refresh)
//     and the HMR websocket.
//
//   - /shared/* (the public, account-less share view, #226): the same policy
//     MINUS external https images. Assistant-authored HTML renders there in a
//     sandbox="" iframe — sandbox blocks scripts but NOT sub-resource loads,
//     so an <img src="//attacker/…"> or CSS @import would beacon every
//     anonymous viewer of the link. A srcdoc iframe inherits this page's CSP,
//     so pinning img-src/style-src/font-src/connect-src to 'self' (+ data:)
//     closes the exfil channel while same-origin workspace images, inline
//     styles, and all markdown text still render.
function contentSecurityPolicy(pathname: string): string {
  const dev = process.env.NODE_ENV === "development";
  const shared = pathname.startsWith("/shared/");
  return [
    "default-src 'self'",
    `script-src 'self' 'unsafe-inline'${dev ? " 'unsafe-eval'" : ""}`,
    "style-src 'self' 'unsafe-inline'",
    `img-src 'self' data: blob:${shared ? "" : " https:"}`,
    "font-src 'self' data:",
    `connect-src 'self'${dev ? " ws: wss:" : ""}`,
    "media-src 'self' data: blob:",
    "frame-src 'self'",
    "object-src 'none'",
    "base-uri 'self'",
    "form-action 'self'",
    "frame-ancestors 'none'",
    "worker-src 'self' blob:",
  ].join("; ");
}

function decorate(res: NextResponse, pathname: string): NextResponse {
  res.headers.set(BUILD_ID_HEADER, currentBuildId());
  res.headers.set("Cache-Control", "no-store, must-revalidate");
  res.headers.set("Content-Security-Policy", contentSecurityPolicy(pathname));
  res.headers.set("X-Frame-Options", "DENY");
  res.headers.set("X-Content-Type-Options", "nosniff");
  res.headers.set("Referrer-Policy", "strict-origin-when-cross-origin");
  res.headers.set("Strict-Transport-Security", "max-age=31536000; includeSubDomains");
  return res;
}

// hasBearer detects moc's username/password Bearer token on the request. The
// token is opaque to Next (the orchestrator at :8000 owns + validates it), so
// this proxy's only job is to NOT block a request that carries one and let
// the upstream proxy authorize it. Without this, a moc bearer user (no cookie)
// would be redirected to /login on every navigation.
function hasBearer(request: NextRequest): boolean {
  const auth = request.headers.get("authorization");
  return !!auth && /^Bearer\s+\S/i.test(auth);
}

export async function proxy(request: NextRequest) {
  const { pathname } = request.nextUrl;

  if (
    pathname.startsWith("/_next") ||
    pathname.startsWith("/favicon") ||
    pathname.startsWith("/icons/") ||
    pathname.startsWith("/logos/") ||
    pathname.startsWith("/backgrounds/") ||
    pathname === "/manifest.webmanifest" ||
    pathname === "/robots.txt"
  ) {
    return NextResponse.next();
  }

  // Public read-only shared conversations (#226): viewable by anyone with the
  // link, logged in or not. Bypass the session gate entirely — and, unlike
  // publicPaths, do NOT bounce a logged-in viewer to /chat, since opening a
  // share link while signed in is legitimate.
  if (pathname.startsWith("/shared/")) {
    return decorate(NextResponse.next(), pathname);
  }

  // Accept either session cookie (elcano_session HMAC or elcano_auth Ed25519).
  const session = await getSessionFromRequest(request);

  if (publicPaths.has(pathname)) {
    if (session) {
      return decorate(NextResponse.redirect(getRedirectUrl(request, "/chat")), pathname);
    }
    return decorate(NextResponse.next(), pathname);
  }

  if (publicApiPaths.has(pathname)) {
    return decorate(NextResponse.next(), pathname);
  }

  // A cookie session OR a moc Bearer admits the request. For Bearer-only
  // (orchestrator API) requests with no cookie, the upstream proxy enforces
  // the real authorization.
  if (!session && !hasBearer(request)) {
    if (pathname.startsWith("/api/")) {
      return decorate(NextResponse.json({ error: "Unauthorized" }, { status: 401 }), pathname);
    }

    return decorate(NextResponse.redirect(getRedirectUrl(request, "/login")), pathname);
  }

  return decorate(NextResponse.next(), pathname);
}

export const config = {
  // Widened from chat's matcher so /orchestrator/* is gated by the SAME rule
  // as /chat/*. Static assets stay excluded (content-hashed, safe to cache).
  matcher: ["/((?!_next/static|_next/image|.*\\.(?:svg|png|jpg|jpeg|gif|webp)$).*)"],
};
