// Thin helpers for talking to chat-server (the local Go agent harness).
//
// The browser never hits chat-server directly — every call goes through a
// Next.js API route that:
//   1. Verifies the session cookie via `getServerSession`.
//   2. Proxies to chat-server with `X-Chat-Server-Token` and `X-User-Email`.
//
// chat-server listens on CHAT_SERVER_URL (default http://127.0.0.1:8080) and
// both sides share CHAT_SERVER_TOKEN.

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
