# Deep links survive sign-in

A Fleet link opened while signed out, or with an expired session, lands where
it pointed once the visitor signs in, instead of on the start page. That
covers the new-chat link `/chat?persona=<ref>&draft=<text>`
([USER-PERSONAS.md](USER-PERSONAS.md)), `/chat?c=<id>`, a team link with its
other parameters ([TEAM-SHARING.md](TEAM-SHARING.md)), a task link
(`/orchestrator/tasks/<id>`, `/orchestrator?task=<id>`), the `/admin` bookmark
aliases, and every other page the SPA renders behind sign-in. The motivating
case is an intake app that opens `<fleet>/chat?draft=…` in a new tab for a
trader whose Fleet session has expired. Nothing a link puts in the composer is
sent until the person presses Send.

The code is `internal/webui/returnto.go`. The request gate in `gateway.go`
calls it, and two pages in `web/` hand it a link of their own.

## What shipped

**Park.** When the gate sends a signed-out `GET` of a page to `/login`, it
stores the page's path and query as the browser sent them in a short-lived
cookie. On https the cookie is `__Host-fleet_return_to`; on plain http (local
and loopback installs) it is `fleet_return_to`. The value is base64url, the
cookie is HttpOnly, SameSite=Lax and `Path=/`, and it lasts 30 minutes, the
same lifetime as the team-link cookie. The `/login` redirect itself is
unchanged.

**Redeem.** Every sign-in leg already ends on `/` with a fresh session: the
password form's 303, the OIDC callback's 303 (`/?lang=…`), and central
Auth's `return_to` after the Elcano email link. So `/` is the one place the
link is redeemed, whichever leg was used, and no leg changed. A signed-in
*bare* landing (`/` or `/chat`, carrying at most the `lang` hint) is sent on
to the parked link, and the cookie is cleared. The landing's `lang` replaces
the link's own; every other byte of the query is kept as sent.

**A newer link wins.** The parked link is replaced or dropped by:

- another signed-out page visit. A visit that cannot be parked (bare `/`,
  bare `/chat`, an over-long link) drops it.
- any Fleet page opened signed in;
- a `/login?return_to=`, even one that cannot be used.

A parked team link follows the same rule. The team cookie now redirects only a
bare landing, and a newer link drops it. Requests that are not page visits
(API calls, an icon a browser fetches on its own, HEAD, POST) leave both
cookies alone. A public `/shared/…` page needs no sign-in, so it leaves the
link for the sign-in still to come.

**Revoked after the page loaded.** The gate's cookie check cannot see a
session the server revoked, for example after a central sign-out. Two pages
therefore send their own link through sign-in as `?return_to=<link>`, which
the gate checks exactly like a parked value: it parks it, or redirects
straight to it when a session exists.

- The chat strips its link's parameters as it boots. It keeps the whole query
  it was opened with (in a ref, so React's development StrictMode replay keeps
  it too) and sends a boot-time 401 to `/login?return_to=<link>`
  (`web/src/app/chat/ui/bootstrapFailure.ts`).
- The Operations Center's sign-in card passes its page to
  `/login?return_to=` and to `/api/orchestrator/auth/elcano-login?return_to=`.
  The latter parks the page and returns the hand-off to `/`.

**The shared `elcano_auth` cookie** still signs a visitor in directly: the
link simply opens, with nothing parked.

## Why a cookie

The gate is the only place that sees the original URL. The sign-in legs are
separate requests that start on `/login`. Carrying the URL through them would
mean changing the login card, all three legs, the OIDC `state` and central
Auth's `return_to`. It would also put the draft text in the IdP's and Auth's
request URLs and logs. A cookie needs no change to any leg, never leaves this
origin, and is the pattern the team-link cookie already used. The cost is
that a link is kept only in the browser that opened it: an Elcano email
sign-in link opened in another browser signs in to the start page there.

## Open-redirect safety

`safeReturnPath` checks a value when it is parked and again when it is read,
because the cookie and `?return_to=` are both input. A value is accepted only
when it meets all of these:

- it starts with `/`;
- it does not start with `//` or `/\`;
- it contains no backslash, space, control or non-ASCII byte;
- it has no scheme, host, userinfo or fragment;
- it is at most 2048 bytes as sent;
- its *decoded* path is a page the SPA renders behind sign-in, or one of its
  bookmark aliases.

`/api/…`, `/login`, `/no-access`, `/shared/…` and unknown paths are refused.
The decoded check is what refuses encoded tricks such as `/%2F%2Fevil`,
`/%5Cevil`, `/chat/%2E%2E/api/x` and double encoding. The redirect is built
with `redirectURL` against the configured origin. On https the `__Host-`
prefix means no sibling subdomain can plant a domain-wide value that this
gate would redeem on every landing and could not delete.

The link is never logged. The one log line is a debug-level note that a link
was too long to keep, and it records only the length.

## Limits and deviations

- **2048 bytes.** A draft of several hundred characters fits even fully
  percent-encoded, and the base64 cookie stays under 2.8 KB, inside a
  browser's 4 KB per-cookie limit. A longer link is not parked, and sign-in
  lands on the start page. The exception is a team link, whose own cookie
  still opens the team chat without the rest of the link.
- **Wider than first asked.** The request was for `/chat` at least. Every page
  parks, because the same rule covers task notification links, which the
  Operations Center guide used to say did not survive sign-in.
- **Team-link changes.** A parked team link is now dropped by a newer link or
  by a page opened signed in. A signed-in `/chat?c=…` with a team link parked
  now opens itself, not the older team chat.
- **One cookie per browser.** Two links opened signed out in two tabs share
  one cookie, so the later one wins.

## Deferred

- The Settings pages' own in-page 401 redirects (`settings/lib/getGated.ts`,
  `useSessionEmail`) still go to plain `/login`, so a Settings link such as
  `/settings/personas?review=<id>` survives an expired session (the gate parks
  it) but not a session the server revokes after the page loaded.
