// CSRF protection for mutating API routes.
//
// Threat model: SameSite=Lax on the session cookie already blocks most
// cross-site POSTs, but it permits top-level-navigation form submits —
// an attacker's page can still do
//
//   <form action="https://chat.example.com/api/auth/login" method="post">
//
// and lure the user into clicking Submit. For the login flow, that's a
// "login CSRF" (attacker signs you into their account). For session-
// authenticated routes, it's classic CSRF.
//
// Defense: compare the request's Origin to our own host — the configured
// canonical origin when the deployment sets one, else Forwarded/
// X-Forwarded-Host. Browsers always set Origin on cross-origin POSTs and
// can't be tricked into forging it. Same-origin requests from our own
// UI carry the right Origin automatically.
//
// Accepted Origin shapes, in order:
//   1. The canonical origin's host (NEXT_PUBLIC_PUBLIC_ORIGIN) when
//      configured — the normal browser → domain path.
//   2. A loopback Origin (`localhost` / `127.0.0.1` / `[::1]`, any port)
//      that exactly matches the CONNECTION's own Host header — the
//      SSH-tunnel shape (browser at http://localhost:3000 forwarded to the
//      box), which a canonical-origin-only check would 403 on every POST
//      including login: an operator lockout that presents as an auth
//      outage. Safe because a victim's browser talking to the real
//      deployment sends the deployment's Host, never loopback, and a page
//      that IS served from the victim's loopback is outside the CSRF
//      threat model (x-forwarded-host is deliberately ignored here, so a
//      forwarded-header attack from a remote origin still fails).
//   3. With no canonical origin configured (local dev, pre-origin
//      deploys): the Forwarded/X-Forwarded-Host → Host fallback chain.
//
// This is deliberately NOT a synchronizer-token pattern — we'd have to
// mint+verify a token across a stateless pre-login flow, which is more
// code to get wrong. Origin enforcement is simpler and equally robust
// against CSRF. For users on pre-SameSite browsers (≪1% of traffic
// today) the check still holds because those browsers still send Origin.

import type { NextRequest } from "next/server";
import { NextResponse } from "next/server";
import { getConfiguredOrigin } from "./auth";

export type CsrfResult = { ok: true } | { ok: false; response: NextResponse };

/**
 * Verify that the request's Origin header matches the host the user is
 * hitting us at. Returns `{ ok: true }` when valid; `{ ok: false, response }`
 * when the handler should short-circuit with the given 403.
 *
 * Called at the top of every mutating (POST/DELETE/PATCH/PUT) API route.
 */
export function verifyOrigin(request: NextRequest): CsrfResult {
  const origin = request.headers.get("origin");
  // Missing Origin on a mutating request is a strong signal of non-browser
  // or stripped traffic — reject. Programmatic callers (curl, Playwright)
  // can set `Origin` explicitly if they need to.
  if (!origin) {
    return { ok: false, response: csrfReject("missing Origin header") };
  }

  // The configured canonical origin outranks the forwarded-host headers
  // (lib/auth.ts#getConfiguredOrigin): x-forwarded-* is client-supplied
  // unless a proxy overwrites it, so trusting it here would let an attacker
  // pick the very host their Origin is compared against. The header chain
  // stays as the fallback so local dev (no configured origin) keeps working.
  const canonicalHost = getConfiguredOrigin()?.host ?? null;
  const expectedHost =
    canonicalHost ??
    request.headers.get("x-forwarded-host") ??
    request.headers.get("host") ??
    request.nextUrl.host;

  let originHost: string;
  try {
    originHost = new URL(origin).host;
  } catch {
    return { ok: false, response: csrfReject("malformed Origin header") };
  }

  if (originHost === expectedHost) {
    return { ok: true };
  }

  // Loopback exception (shape 2 in the header comment): an operator reaching
  // a canonical-origin box over an SSH tunnel browses at
  // http://localhost:<port>, so their Origin can never equal the canonical
  // host. Accept it only when the Origin exactly matches the host the
  // request actually arrived at AND that host is loopback. The direct Host
  // header — never x-forwarded-host, which is client-supplied — is compared,
  // so a remote page can't reach this branch: its victim's browser sends the
  // deployment's real Host, and a mismatched local port (another dev server
  // on the operator's machine) still fails the equality check.
  if (canonicalHost !== null) {
    const connectionHost = request.headers.get("host") ?? request.nextUrl.host;
    if (originHost === connectionHost && isLoopbackHost(connectionHost)) {
      return { ok: true };
    }
  }

  return {
    ok: false,
    response: csrfReject(
      `origin ${originHost} does not match expected host ${expectedHost}` +
        (canonicalHost !== null
          ? " (canonical origin is configured; loopback tunnel origins matching the connection host are also accepted)"
          : ""),
    ),
  };
}

// isLoopbackHost reports whether a host[:port] names the loopback interface
// in one of the exact shapes a tunneled browser produces: `localhost`,
// `127.0.0.1`, or `[::1]`, with any port. Deliberately not a DNS lookup and
// not a suffix match — `localhost.evil.example` must not qualify.
function isLoopbackHost(host: string): boolean {
  // URL.host and the Host header both carry IPv6 literals in brackets.
  const hostname = host.startsWith("[")
    ? host.slice(0, host.indexOf("]") + 1)
    : host.split(":")[0];
  return (
    hostname === "localhost" || hostname === "127.0.0.1" || hostname === "[::1]"
  );
}

function csrfReject(reason: string): NextResponse {
  // Don't leak the specific reason to the caller — attackers don't need
  // the hint. We do log server-side for debugging.
  console.warn(`csrf rejected: ${reason}`);
  return NextResponse.json({ error: "forbidden" }, { status: 403 });
}
