// Thin helpers for talking to the orchestrator HTTP server (was moc, the Go
// fleet orchestrator listener on :8000).
//
// The browser never hits the orchestrator directly — every call goes through a
// Next.js API route under /api/orchestrator/* that:
//   1. Either verifies an elcano session cookie (getServerSession) OR forwards
//      a moc username/password Bearer token from the incoming request.
//   2. Proxies to the orchestrator, injecting the user's identity.
//
// The orchestrator listens on ORCHESTRATOR_SERVER_URL (default
// http://127.0.0.1:8000). Both login paths are supported:
//   - elcano cookie  → forwarded as X-User-Email (+ the shared server token,
//     mirroring chatServer's X-Chat-Server-Token convention).
//   - moc bearer     → forwarded verbatim as Authorization: Bearer <token>.

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

export type OrchestratorAuth =
  | { kind: "cookie"; email: string }
  | { kind: "bearer"; token: string };

// Build the upstream auth headers from whichever credential the browser
// presented. A moc bearer wins when present (it's the explicit moc login); the
// elcano cookie is the fallback.
export function orchestratorHeaders(auth: OrchestratorAuth, extra?: HeadersInit): Headers {
  const h = new Headers(extra ?? {});
  if (auth.kind === "bearer") {
    h.set("Authorization", `Bearer ${auth.token}`);
  } else {
    const token = getOrchestratorSharedToken();
    if (token) h.set("X-Orchestrator-Server-Token", token);
    h.set("X-User-Email", auth.email);
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
  return fetch(`${base}${path}`, {
    ...init,
    headers,
    cache: "no-store",
  });
}
