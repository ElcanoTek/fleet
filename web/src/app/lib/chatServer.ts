// Thin helpers for talking to chat-server (the local Go agent harness).
//
// The browser never hits chat-server directly — every call goes through a
// Next.js API route that:
//   1. Verifies the session cookie via `getServerSession`.
//   2. Proxies to chat-server with `X-Chat-Server-Token`, `X-User-Email` and
//      the session-epoch claim.
//
// chat-server listens on CHAT_SERVER_URL (default http://127.0.0.1:8080) and
// both sides share CHAT_SERVER_TOKEN.

import { cookies } from "next/headers";
import { NextResponse } from "next/server";

import { getSessionCookieName } from "@/app/lib/auth";
import { forwardedHeaders } from "@/app/lib/proxyHeaders";

const defaultBase = "http://127.0.0.1:8080";

/**
 * What a proxied call forwards about its caller. A `Session` satisfies it
 * structurally, which is why every call site hands over the whole session
 * rather than `session.email`: the epoch is half of the identity chat-server
 * checks, and a bare email would silently drop it.
 *
 * `epoch` is absent only where there is no chat-minted session to read it from
 * — the pre-login /auth/verify and /auth/session-epoch calls, and elcano_auth
 * sessions, whose cookie the auth service owns.
 */
export type SessionIdentity = {
  email: string;
  epoch?: string;
};

/**
 * Set by chat-server on the 401 it returns when a forwarded epoch no longer
 * matches the account (internal/httpapi/auth.go#writeSessionRevoked). Mirrored
 * here rather than shared because the two tiers are separate builds.
 */
const sessionRevokedHeader = "X-Session-Revoked";

export function getChatServerBase() {
  return (process.env.CHAT_SERVER_URL ?? defaultBase).replace(/\/+$/, "");
}

export function getSharedToken() {
  const t = process.env.CHAT_SERVER_TOKEN;
  if (!t) {
    throw new Error("Missing required environment variable: CHAT_SERVER_TOKEN");
  }
  return t;
}

export function chatServerHeaders(user: SessionIdentity, extra?: HeadersInit): Headers {
  const h = new Headers(extra ?? {});
  h.set("X-Chat-Server-Token", getSharedToken());
  h.set("X-User-Email", user.email);
  if (user.epoch) h.set("X-User-Session-Epoch", user.epoch);
  return h;
}

/**
 * fetchSessionEpoch reads the account's current session epoch so a cookie about
 * to be minted can carry it. Both mint paths (the password form and the OIDC
 * callback) go through here.
 *
 * Returns null when chat-server cannot answer. The caller MUST then refuse to
 * log the user in: a cookie minted without an epoch is rejected by
 * verifySessionToken on the very next request, so falling back would strand the
 * user in a login loop rather than degrade gracefully.
 */
export async function fetchSessionEpoch(email: string): Promise<string | null> {
  let upstream: Response;
  try {
    upstream = await chatServerFetch({ email }, "/auth/session-epoch", { method: "GET" });
  } catch {
    return null;
  }
  if (!upstream.ok) return null;
  const body = (await upstream.json()) as { session_epoch?: string };
  return body.session_epoch || null;
}

/**
 * dropRevokedSession deletes the session cookie when chat-server reports that
 * the epoch it was minted against is stale.
 *
 * Without this the user is trapped: the cookie's signature is still valid, so
 * the request proxy treats it as a session and bounces every visit to /login
 * back to /chat, which 401s again. The Next tier cannot detect the staleness
 * itself — the epoch lives in the users table — so chat-server's verdict is the
 * only trigger available.
 *
 * Cookie writes are legal in a Route Handler but not while rendering a Server
 * Component, and that is why the failure is swallowed: the server-rendered
 * callers (chat/page.tsx) only ever proxy for elcano sessions, which carry no
 * epoch and so can never receive this verdict, and any client-side call that
 * follows is a Route Handler that can complete the deletion.
 */
async function dropRevokedSession(upstream: Response): Promise<void> {
  if (upstream.status !== 401 || upstream.headers.get(sessionRevokedHeader) !== "1") return;
  try {
    (await cookies()).delete(getSessionCookieName());
  } catch {
    // Not in a context that may mutate cookies — see above.
  }
}

/**
 * Fetch from chat-server; throws on network errors, but passes through
 * non-2xx responses so the caller can forward status codes to the browser.
 *
 * Most routes should prefer `chatServerProxy`, which converts the connection
 * error into a clean 502 instead of letting it bubble to a generic 500 page.
 */
export async function chatServerFetch(
  user: SessionIdentity,
  path: string,
  init?: RequestInit,
): Promise<Response> {
  const base = getChatServerBase();
  const headers = chatServerHeaders(user, init?.headers);
  if (init?.body && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  const upstream = await fetch(`${base}${path}`, {
    ...init,
    headers,
    // Keep the request open for long-running SSE streams.
    cache: "no-store",
  });
  // Status + header only: the body is never read here, so streaming callers
  // (SSE, the download passthroughs) still forward it untouched.
  await dropRevokedSession(upstream);
  return upstream;
}

/**
 * chatServerFetchPublic fetches a chat-server endpoint that requires the shared
 * secret but NO user identity (#226 read-only sharing). It sends only
 * `X-Chat-Server-Token` — never `X-User-Email` — so it can serve logged-out
 * viewers of /shared/{token} without impersonating a user. The Go endpoint is
 * token-gated (only this trusted proxy reaches it) but identity-less; the share
 * token in the path is the authorization.
 */
export async function chatServerFetchPublic(path: string, init?: RequestInit): Promise<Response> {
  const base = getChatServerBase();
  const headers = new Headers(init?.headers ?? {});
  headers.set("X-Chat-Server-Token", getSharedToken());
  return fetch(`${base}${path}`, { ...init, headers, cache: "no-store" });
}

/**
 * chatServerProxy wraps chatServerFetch and converts a CONNECTION failure
 * (chat-server down/restarting → fetch throws) into a clean 502 JSON response
 * instead of letting the thrown error bubble into Next.js's generic 500 HTML
 * page. The streaming/chat routes already return this shape; this lets the
 * non-streaming proxy routes return it too.
 *
 * It returns a discriminated result so STREAMING callers (summarize, export)
 * can still pipe `upstream.body` — on success it returns the RAW Response
 * WITHOUT reading the body, so the caller forwards status/body/headers exactly
 * as before; on failure it returns `error`, a NextResponse to return directly.
 * A non-2xx upstream is NOT an error here — it is forwarded verbatim, same as
 * chatServerFetch.
 */
export async function chatServerProxy(
  user: SessionIdentity,
  path: string,
  init?: RequestInit,
): Promise<{ upstream: Response; error?: undefined } | { upstream?: undefined; error: NextResponse }> {
  try {
    const upstream = await chatServerFetch(user, path, init);
    return { upstream };
  } catch (err) {
    return {
      error: NextResponse.json(
        { error: `chat-server unreachable: ${(err as Error).message}` },
        { status: 502 },
      ),
    };
  }
}

/**
 * chatServerPassthrough — the full proxy-route body: chatServerProxy, then
 * re-emit the upstream response verbatim (status + body + forwarded headers). The
 * admin proxy routes each used to inline this trio; a passthrough fix (header
 * forwarding, error shaping) belongs here, once.
 *
 * The body is STREAMED rather than buffered through `await upstream.text()`.
 * Nothing here inspects it, and buffering held the whole payload in memory on a
 * single-box deployment while defeating the streaming writers upstream
 * (`csv.NewWriter(w)`, `json.NewEncoder(w)`). It also meant any future binary
 * response through this funnel would be silently corrupted — the failure two
 * sibling routes already carry comments about avoiding.
 */
export async function chatServerPassthrough(
  user: SessionIdentity,
  path: string,
  init?: RequestInit,
): Promise<NextResponse> {
  const { upstream, error } = await chatServerProxy(user, path, init);
  if (error) return error;
  // 204/304 and HEAD have no body; NextResponse requires null, not an empty stream.
  return new NextResponse(upstream.body, {
    status: upstream.status,
    headers: forwardedHeaders(upstream),
  });
}
