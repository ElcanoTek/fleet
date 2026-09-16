// Pure classifier for a failed bootstrap/revalidation fetch, shared by the
// chat experience (chat-experience.tsx) and the Operations Center session
// probe (shared/hooks/useOrchestratorSession.ts); exported separately so
// vitest can pin the contract without booting React.
//
// The distinction is load-bearing: /api/conversations proxies to the Go
// backend, so it answers 502/503/504 whenever that process is down or
// restarting — with the session cookie still perfectly valid. Redirecting to
// /login on those (as "session expired") loops: the middleware sees the valid
// cookie on /login and bounces straight back to /chat, which fails the same
// fetch again, forever. Only a real auth verdict (401/403) may redirect;
// everything else — backend errors and thrown network failures alike — must
// surface as "can't reach the chat server" instead. The Operations Center has
// the same shape in miniature: its /me probe answers 500 when the fail-closed
// session-epoch lookup can't reach the chat DB, and treating that as
// signed-out swapped the dashboard for a login card mid-incident.

//
// 401 and 403 are different verdicts and go to different places. 401 means no
// (valid) session: sign in again. 403 means a valid session whose identity is
// not on this deployment's user-list — the shared elcano_auth cookie or a
// central-Auth (OIDC) sign-in for someone Fleet has not provisioned. Sending
// that person to /login is the loop of #1522 (the proxy sees the valid cookie
// and bounces them back to /chat); they belong on /no-access, which explains
// the situation and offers the sign-out that also ends the central session.

export type BootstrapFailure = "unauthenticated" | "forbidden" | "unreachable";

export function classifyBootstrapFailure(status: number): BootstrapFailure {
  if (status === 401) return "unauthenticated";
  if (status === 403) return "forbidden";
  return "unreachable";
}

// bootstrapFailureDestination is where the browser must go for a failure that
// IS an auth verdict, or null when it is not (backend unreachable: stay put
// and say so). Call sites navigate with location.replace so the failed page
// does not linger in history behind the destination.
export function bootstrapFailureDestination(
  status: number,
): "/login" | "/no-access" | null {
  switch (classifyBootstrapFailure(status)) {
    case "unauthenticated":
      return "/login";
    case "forbidden":
      return "/no-access";
    default:
      return null;
  }
}
