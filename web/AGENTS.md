# web/ — the fleet web UI (agent notes)

Read the repo-root [`AGENTS.md`](../AGENTS.md) first: its invariants and the
Elcano frontend standard apply here. This file covers only what is specific to
`web/`.

## What this is

A **Tier B** single-page app: React 19 + TypeScript 7, built by Vite into
`../internal/webui/bundle/dist`, which the `fleet` binary embeds with
`go:embed` and serves itself ([ADR-0091](../docs/adr/0091-go-serves-the-browser-tier.md)).
There is no Node server in production and no server-side rendering; nothing
in `web/` runs on the server.

- **Go owns the server side** (`internal/webui`): the auth gate, sessions,
  CSRF, OIDC, CSP and security headers, `X-App-Version`, theme/brand/manifest,
  and every `/api/*` route (called in-process into the chat and orchestrator
  handlers). A new `/api/*` route is added there, not here.
- **Go renders `index.html` per request**: the page `<title>` and share
  metadata, and a JSON bootstrap (`<script type="application/json"
  id="fleet-bootstrap">`: branding, session email, flags) read by
  `src/app/lib/bootstrap.ts`. The CSP is `script-src 'self'` — never add an
  inline script; `public/theme-init.js` is the one pre-paint script and is a
  file for that reason.
- **Routing is in-house**: `src/App.tsx` is the route table (lazy pages),
  `src/app/lib/navigation.tsx` provides `usePathname`/`useRouter`/`navigate`,
  `src/app/lib/Link.tsx` the link component. Paths that need a server-side
  decision (`/` → `/chat`, `/admin`, the `/chat` membership gate, a missing
  `/shared/{token}`) are redirected by Go before the page is served. Keep
  URLs stable: `/chat`, `/orchestrator`, `/settings/...`, `/admin`,
  `/help/...`, `/shared/{token}`, `/login`, `/no-access`.

## Commands (run from `web/`; verified)

```sh
npm ci                              # lockfile-exact install
npm run lint                        # oxlint --deny-warnings
npm run typecheck                   # tsc --noEmit — the ONLY type check (vite build strips types)
npm run test                        # vitest
npm run build                       # vite build → ../internal/webui/bundle/dist
npm run test:e2e:mocked             # Playwright, mocked: builds the bundle + cmd/web-e2e-server, then drives them
```

From the repo root: `make ci-web` (the CI job verbatim, ending with a binary
that must embed the bundle), `make ci-e2e-mocked`, and `make dev` — `fleet
serve` with `FLEET_WEB_DEV_SERVER=http://127.0.0.1:5173` so the real Go gate
and API sit in front of the Vite dev server (hot reload; open
`http://127.0.0.1:3000`).

## Runtime dependencies (each justified, per the standard)

| Package | Why |
|---|---|
| `react`, `react-dom` | the Tier B framework |
| `react-markdown`, `remark-gfm` | assistant and guide Markdown (GFM tables, task lists) without `dangerouslySetInnerHTML` |
| `@tanstack/react-virtual` | virtualised long chat transcripts (`ChatTranscript.tsx`) |

Code highlighting is in-housed (`src/app/chat/ui/highlight/`, see the root
`AGENTS.md`). Add a runtime dependency only with a row here and a reason.

## UI components and design

- **New or replaced base components come from Flag's shadcn/ui registry**
  (`npx shadcn add <registry-url>/r/<item>.json`; the registry URL is in
  Flag's README). The existing components are fleet's own and are not being
  migrated wholesale; build one by hand only when the registry lacks it, then
  upstream it to Flag.
- Design rules (tokens, contrast, typefaces): [`src/app/DESIGN.md`](src/app/DESIGN.md).
- **Vendored Flag files** — `src/app/fonts/` (whole dir),
  `public/icons/core-icons.svg`, `public/logos/fleet-mark.svg` and
  `src/app/FLAG-LICENSE` — are listed with hashes in the root `flag.lock`.
  Never edit them: `scripts/flag-sync.sh check` (part of `make stack-check`)
  fails on any change. Update with `scripts/flag-sync.sh sync ../flag` from a
  clean sibling Flag checkout, then commit the files and `flag.lock` together.
  Icons Flag does not ship yet live in `public/icons/fleet-icons.svg`;
  `src/app/shared/ui/iconHref.ts` picks the sprite per id.

## Tests

- vitest beside the code (`*.test.ts(x)`), jsdom. Mocks of
  `@/app/lib/navigation` stub only the hooks; `Link` lives in its own module.
- Playwright (`e2e/`): `mocked` (deterministic, the Go gateway alone with
  route-intercepted backends — includes the axe accessibility spec), `live`
  (real stack + sandbox, fake LLM), `canary`, `screenshots`. See
  [`e2e/README.md`](e2e/README.md). Never run `playwright install` in a
  sandboxed agent session; use the browsers the environment provides.
