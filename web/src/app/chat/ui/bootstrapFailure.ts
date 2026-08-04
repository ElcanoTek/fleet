// Pure classifier for a failed bootstrap/revalidation fetch in the chat
// experience (chat-experience.tsx), exported separately so vitest can pin the
// contract without booting React.
//
// The distinction is load-bearing: /api/conversations proxies to the Go
// backend, so it answers 502/503/504 whenever that process is down or
// restarting — with the session cookie still perfectly valid. Redirecting to
// /login on those (as "session expired") loops: the middleware sees the valid
// cookie on /login and bounces straight back to /chat, which fails the same
// fetch again, forever. Only a real auth verdict (401/403) may redirect;
// everything else — backend errors and thrown network failures alike — must
// surface as "can't reach the chat server" instead.

export type BootstrapFailure = "unauthenticated" | "unreachable";

export function classifyBootstrapFailure(status: number): BootstrapFailure {
  return status === 401 || status === 403 ? "unauthenticated" : "unreachable";
}
