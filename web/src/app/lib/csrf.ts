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
// Defense: compare the request's Origin (or Forwarded/X-Forwarded-Host)
// to our own host. Browsers always set Origin on cross-origin POSTs and
// can't be tricked into forging it. Same-origin requests from our own
// UI carry the right Origin automatically.
//
// This is deliberately NOT a synchronizer-token pattern — we'd have to
// mint+verify a token across a stateless pre-login flow, which is more
// code to get wrong. Origin enforcement is simpler and equally robust
// against CSRF. For users on pre-SameSite browsers (≪1% of traffic
// today) the check still holds because those browsers still send Origin.

import type { NextRequest } from "next/server";
import { NextResponse } from "next/server";

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

  const expectedHost =
    request.headers.get("x-forwarded-host") ??
    request.headers.get("host") ??
    request.nextUrl.host;

  let originHost: string;
  try {
    originHost = new URL(origin).host;
  } catch {
    return { ok: false, response: csrfReject("malformed Origin header") };
  }

  if (originHost !== expectedHost) {
    return {
      ok: false,
      response: csrfReject(`origin ${originHost} does not match host ${expectedHost}`),
    };
  }
  return { ok: true };
}

function csrfReject(reason: string): NextResponse {
  // Don't leak the specific reason to the caller — attackers don't need
  // the hint. We do log server-side for debugging.
  console.warn(`csrf rejected: ${reason}`);
  return NextResponse.json({ error: "forbidden" }, { status: 403 });
}
