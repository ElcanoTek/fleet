// The session-revocation verdict, shared by both backend proxies.
//
// One elcano_session cookie reaches two backends — chat-server (chatServer.ts)
// and the orchestrator (mocServer.ts) — so both forward the cookie's session
// epoch and both can answer "that session is no longer valid". The wire
// contract and the browser-side consequence live here once, because a funnel
// that recognised the verdict but left the cookie in place would trap the user.

import { cookies } from "next/headers";

import { getSessionCookieName } from "@/app/lib/auth";

/**
 * Set by either backend on the 401 it returns when a forwarded epoch no longer
 * matches the account (internal/httpapi/auth.go#writeSessionRevoked,
 * internal/sched/handlers/session_epoch.go#checkSessionEpoch). Mirrored here
 * rather than shared because the tiers are separate builds.
 */
const sessionRevokedHeader = "X-Session-Revoked";

/**
 * dropRevokedSession deletes the session cookie when a backend reports that the
 * epoch it was minted against is stale.
 *
 * Without this the user is trapped: the cookie's signature is still valid, so
 * the request proxy treats it as a session and bounces every visit to /login
 * back to /chat, which 401s again. The Next tier cannot detect the staleness
 * itself — the epoch lives in the chat users table — so the backend's verdict is
 * the only trigger available.
 *
 * Status + header only: the body is never read, so streaming callers (SSE, the
 * download passthroughs) still forward it untouched.
 *
 * Cookie writes are legal in a Route Handler but not while rendering a Server
 * Component, and that is why the failure is swallowed: the server-rendered
 * callers (chat/page.tsx) only ever proxy for elcano sessions, which carry no
 * epoch and so can never receive this verdict, and any client-side call that
 * follows is a Route Handler that can complete the deletion.
 */
export async function dropRevokedSession(upstream: Response): Promise<void> {
  if (upstream.status !== 401 || upstream.headers.get(sessionRevokedHeader) !== "1") return;
  try {
    (await cookies()).delete(getSessionCookieName());
  } catch {
    // Not in a context that may mutate cookies — see above.
  }
}
