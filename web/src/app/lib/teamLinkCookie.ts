// The signed-out leg of a team link (docs/TEAM-SHARING.md, B24).
//
// A team link is `/chat?team=<conversation id>`. A visitor without a session
// is sent to sign in first, and must land on that chat afterwards — but the
// sign-in paths (password form, SSO, Elcano email) all finish by redirecting
// home, and none of them carries a return URL. So the proxy parks the id in a
// short-lived cookie on the way to /login and, on the first signed-in request
// to `/` or `/chat` that carries it, redirects to the chat and deletes it.
//
// The id is validated strictly in both directions: it is a server-minted UUID,
// and anything else is dropped rather than echoed into a redirect URL.

export const TEAM_LINK_COOKIE = "fleet_team_link";

/** Thirty minutes: long enough to sign in, short enough to not surprise later. */
export const TEAM_LINK_MAX_AGE_SECONDS = 1800;

const UUID =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** A conversation id safe to park in the cookie and put back in a URL. */
export function isTeamLinkId(value: string | null | undefined): value is string {
  return typeof value === "string" && UUID.test(value);
}
