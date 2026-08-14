// Thin helpers for talking to the orchestrator HTTP server (listener on :8000).
//
// The browser never hits the orchestrator directly — every call goes through a
// Next.js API route under /api/orchestrator/* that:
//   1. Either verifies an elcano session cookie (getServerSession) OR forwards
//      a username/password Bearer token from the incoming request.
//   2. Proxies to the orchestrator, injecting the user's identity.
//
// The orchestrator listens on ORCHESTRATOR_SERVER_URL (default
// http://127.0.0.1:8000). Both login paths are supported:
//   - elcano cookie  → forwarded as X-User-Email + the session-epoch claim
//     (+ the shared server token, mirroring chatServer's X-Chat-Server-Token
//     convention).
//   - bearer         → forwarded verbatim as Authorization: Bearer <token>.
//
// The bearer path is the leftover moc username/password login; it will be
// removed in the wave-2 operator-honesty pass (cookie/OIDC is the real path).

import { dropRevokedSession } from "@/app/lib/sessionRevocation";

const defaultBase = "http://127.0.0.1:8000";

export function getOrchestratorBase() {
  return (process.env.ORCHESTRATOR_SERVER_URL ?? defaultBase).replace(/\/+$/, "");
}

// The shared token that proves a cookie-path request came from THIS Next layer
// (which has already verified the user's session). The orchestrator backend
// trusts the forwarded X-User-Email only when this token matches (#157), exactly
// as chat-server trusts X-Chat-Server-Token. It is the SAME secret chat uses:
// fleet runs both backends in one process with one config, so reusing
// CHAT_SERVER_TOKEN avoids a second secret with no security benefit. A distinct
// ORCHESTRATOR_SERVER_TOKEN still wins if explicitly set.
export function getOrchestratorSharedToken(): string | undefined {
  return process.env.ORCHESTRATOR_SERVER_TOKEN || process.env.CHAT_SERVER_TOKEN || undefined;
}

// The cookie variant carries the session epoch as well as the email, because
// the orchestrator gates on BOTH: one elcano_session cookie reaches both
// backends, so dropping the claim here would leave a password reset evicting
// the cookie from chat while the Operations Center kept honouring it. `epoch`
// is absent for an elcano_auth (magic-link) session, whose cookie the auth
// service mints and revokes.
export type OrchestratorAuth =
  | { kind: "cookie"; email: string; epoch?: string }
  | { kind: "bearer"; token: string };

// Build the upstream auth headers from whichever credential the browser
// presented. A bearer token wins when present (the explicit password login); the
// elcano cookie is the fallback.
export function orchestratorHeaders(auth: OrchestratorAuth, extra?: HeadersInit): Headers {
  const h = new Headers(extra ?? {});
  if (auth.kind === "bearer") {
    h.set("Authorization", `Bearer ${auth.token}`);
  } else {
    const token = getOrchestratorSharedToken();
    if (token) h.set("X-Orchestrator-Server-Token", token);
    h.set("X-User-Email", auth.email);
    if (auth.epoch) h.set("X-User-Session-Epoch", auth.epoch);
  }
  return h;
}

/**
 * Fetch from the orchestrator; throws on network errors, but passes through
 * non-2xx responses so the caller can forward status codes to the browser.
 */
export async function orchestratorFetch(
  auth: OrchestratorAuth,
  path: string,
  init?: RequestInit,
): Promise<Response> {
  const base = getOrchestratorBase();
  const headers = orchestratorHeaders(auth, init?.headers);
  if (init?.body && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  const upstream = await fetch(`${base}${path}`, {
    ...init,
    headers,
    cache: "no-store",
  });
  // Status + header only: the body is never read here, so the streaming callers
  // (the task run-log SSE, the CSV/workspace passthroughs) forward it untouched.
  await dropRevokedSession(upstream);
  return upstream;
}
