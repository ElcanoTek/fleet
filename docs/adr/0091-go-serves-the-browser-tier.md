# ADR-0091: The fleet binary serves the browser tier; the web UI is a Vite SPA embedded in it

- **Status:** Accepted
- **Date:** 2026-10-22
- **Deciders:** fleet maintainers
- **Supersedes:** the Next.js web tier (`fleet-web.service`, the Helm `web`
  Deployment). [ADR-0014](0014-oidc-sso-in-nextjs.md) (OIDC SSO) stands, but
  its implementation now lives in `internal/webui/oidc.go`, not in Next.js.
- **Relates to:** [ADR-0053](0053-public-api-through-the-tls-front.md) (the
  public API through the TLS front),
  [ADR-0041](0041-mandatory-session-epoch-claim.md) and
  [ADR-0064](0064-application-session-lifetimes.md) (session cookie),
  the Elcano frontend standard (the block at the top of `AGENTS.md`).

## Context

Until now the browser never spoke to Go. It spoke to a Next.js 16 server
(`web/`, run by `fleet-web.service` on 127.0.0.1:3000), whose route handlers
(`web/src/app/api/**`) verified the session cookie, enforced CSRF, set the
security headers and then proxied each call over loopback HTTP to the chat
listener (`X-Chat-Server-Token` + `X-User-Email`) or the orchestrator
(`X-Orchestrator-Server-Token`). That meant:

- a second runtime in production (node, a separate unit, its own env file,
  its own crash and shutdown behaviour — `docs/WEB-TIER-SHUTDOWN.md` existed
  only because of it), and a node interpreter every box had to keep current
  at run time;
- two implementations of sign-in: the Go side minted nothing but verified the
  Ed25519 `elcano_auth` cookie, while the HMAC `elcano_session` cookie, OIDC,
  back-channel logout and the session-epoch checks were TypeScript;
- the Elcano frontend standard, which every other product follows, says the
  server is Go and serves everything, and that an interactive app is a static
  React bundle embedded in that server (Tier B).

## Decision

1. **`internal/webui` is the browser gateway, in the fleet process.** It
   listens on `FLEET_WEB_ADDR` (default `127.0.0.1:3000`, the port the Next
   tier used, so Caddy and every existing Caddyfile are unchanged; `off`
   disables it). It serves every `/api/*` path the Next tier served, with the
   same methods, paths and response shapes — `internal/webui/testdata/
   next-api-routes.txt` is the inventory and `TestEveryNextRouteIsServed`
   fails on a missing one; an unknown `/api` path is a JSON 404 (405 with
   `Allow` for a wrong method).
2. **Backends are called in-process.** The gateway invokes the chat server's
   and the orchestrator's `http.Handler`s directly with freshly built trust
   headers (shared token + identity, or a passed-through bearer for the
   orchestrator's machine clients) and a loopback `RemoteAddr`; it never
   forwards a browser's own trust headers. The header-trust channel is
   therefore still reachable from loopback only, and Caddy still strips it on
   the public API paths (ADR-0053).
3. **One implementation of sign-in.** `internal/websession` holds the HMAC
   session cookie (same format, same `APP_SESSION_SECRET`, so sessions
   minted by the Next tier stay valid across the upgrade) and the Ed25519
   `elcano_auth` verification that `internal/sched/handlers` now delegates to.
   The gateway owns the auth gate and public allowlists, the CSRF origin check,
   OIDC (PKCE, ID-token validation, back-channel logout), CSP and the other
   security headers, `X-App-Version`, and the theme/brand/manifest routes.
4. **The web UI is a Vite + React SPA** (`web/`). `vite build` writes
   `internal/webui/bundle/dist`, which the binary embeds with `go:embed`.
   Go renders `index.html` per request — page title and share metadata, plus
   a JSON bootstrap (branding, session, flags) in a non-executable
   `<script type="application/json">` — so the CSP is `script-src 'self'`
   with no inline script. Hashed `/assets/*` are `immutable`; other static
   files revalidate by ETag; HTML is `no-store`. Server-side redirects the
   Next pages did (`/` → `/chat`, `/admin`, the membership gate, the
   `/shared/{token}` 404) are done in Go before the page is served.
5. **Routing is in-house** (`web/src/app/lib/navigation.tsx`, `Link.tsx`):
   the app needs path matching for a dozen routes, `usePathname`/`useRouter`
   equivalents and history integration — about 150 lines. A router package
   would be the largest runtime dependency in the bundle for that, and the
   standard asks for few, justified runtime dependencies.
6. **`go build` and `go test` work without the bundle.** The embed is
   `all:bundle` over a committed `bundle/README.md`; a binary built without
   `make web` serves a 503 page (the API still works) and `fleet version
   --web` exits 1. `make build` always builds the bundle first and ends with
   that check, and so do `scripts/update.sh`, `bootstrap.sh`,
   `fleet-upgrade.sh` and the CI web job — a deployed binary always carries
   the real bundle.

## Consequences

- **No node at run time.** Node is a build dependency only (`make build`
  runs `npm ci && vite build`). `FLEET_NODE_BIN` and the start shim are gone.
- **Upgrade path, single box.** `fleet update` builds the new binary,
  copies the browser keys (`APP_SESSION_SECRET`, `AUTH_*`, `FLEET_OIDC_*`,
  `CATWALK_URL`, a non-default bind as `FLEET_WEB_ADDR`) from
  `/etc/fleet/fleet-web.env` into `fleet.env` when absent, then stops and
  disables `fleet-web.service` before restarting fleet, which binds :3000
  itself, and removes the old unit, drop-in, start shim and `/opt/fleet/web`
  only once fleet is back up (`fleet-upgrade.sh` waits for its readiness
  gate, and its rollback to the previous binary restarts the old tier). The
  key list and the stop/remove/restore steps live in one place,
  `scripts/lib/web-tier.sh`, shared by update, bootstrap, doctor and
  fleet-upgrade.
  `fleet-web.env` and the `fleet-web` user are left for the operator to
  delete. `fleet doctor` performs the same retirement on a box updated by
  hand and repairs a missing `APP_SESSION_SECRET`. Every path keeps that
  order — the old tier's files go only after fleet is up — and a bootstrap
  re-run whose restart fails restores the old tier and exits non-zero instead
  of carrying on (`fleet_cutover_web_tier`).
- **Upgrade path, Kubernetes.** The chart has no `web` Deployment; the fleet
  pod exposes the web port and the Ingress points at it. A values file that
  still sets `web.enabled=true` fails to render with the steps to take
  (`deploy/helm/fleet/README.md`).
- **Rolling back** to a release before this one needs the old web tier back
  (`git checkout <old tag> && sudo scripts/bootstrap.sh --enable-web`), since
  an older binary serves no UI.
- Browser requests reach the backends as loopback, exactly as through the
  Next proxy, so per-client rate limits and IP lists still cannot tell
  browsers apart; filter those at the TLS front.
- The TypeScript route-handler tests became Go tests in `internal/webui`
  and `internal/websession`; the mocked Playwright suite drives the Go gateway
  (`cmd/web-e2e-server`) serving the built bundle.
