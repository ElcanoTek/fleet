// Thin helpers for talking to chat-server (the local Go agent harness).
//
// The browser never hits chat-server directly — every call goes through a
// Next.js API route that:
//   1. Verifies the session cookie via `getServerSession`.
//   2. Proxies to chat-server with `X-Chat-Server-Token` and `X-User-Email`.
//
// chat-server listens on CHAT_SERVER_URL (default http://127.0.0.1:8080) and
// both sides share CHAT_SERVER_TOKEN.

import { NextResponse } from "next/server";

const defaultBase = "http://127.0.0.1:8080";

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

export function chatServerHeaders(userEmail: string, extra?: HeadersInit): Headers {
  const h = new Headers(extra ?? {});
  h.set("X-Chat-Server-Token", getSharedToken());
  h.set("X-User-Email", userEmail);
  return h;
}

/**
 * Fetch from chat-server; throws on network errors, but passes through
 * non-2xx responses so the caller can forward status codes to the browser.
 *
 * Most routes should prefer `chatServerProxy`, which converts the connection
 * error into a clean 502 instead of letting it bubble to a generic 500 page.
 */
export async function chatServerFetch(
  userEmail: string,
  path: string,
  init?: RequestInit,
): Promise<Response> {
  const base = getChatServerBase();
  const headers = chatServerHeaders(userEmail, init?.headers);
  if (init?.body && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  return fetch(`${base}${path}`, {
    ...init,
    headers,
    // Keep the request open for long-running SSE streams.
    cache: "no-store",
  });
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
  userEmail: string,
  path: string,
  init?: RequestInit,
): Promise<{ upstream: Response; error?: undefined } | { upstream?: undefined; error: NextResponse }> {
  try {
    const upstream = await chatServerFetch(userEmail, path, init);
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
