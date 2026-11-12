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
  URLs stable: `/chat`, `/orchestrator`, `/analytics`, `/settings/...`, `/admin`,
  `/help/...`, `/shared/{token}`, `/login`, `/no-access`.

## Commands (run from `web/`; verified)

```sh
npm ci                              # lockfile-exact install
npm run lint                        # oxlint --deny-warnings --report-unused-disable-directives
npm run typecheck                   # tsc --noEmit — the ONLY type check (vite build strips types)
npm run test                        # vitest
npm run build                       # vite build → ../internal/webui/bundle/dist
npm run test:e2e:mocked             # Playwright, mocked: builds the bundle + cmd/web-e2e-server, then drives them
npm run gen:api                     # src/api/generated/ from ../docs/openapi.yaml (openapi-typescript)
```

**Generated API types.** `src/api/generated/` is generated from
`docs/openapi.yaml` (itself generated from the Go router; ADR-0109,
[`docs/HTTP-CONTRACTS.md`](../docs/HTTP-CONTRACTS.md)); never edit it. A wire
type the spec covers is an alias of `components["schemas"][...]` (see
`src/app/shared/lib/orchestratorApi.ts`), not a hand-written mirror. The
generator lives in the `tools/openapi-typescript` workspace because it needs
TypeScript 5's compiler API; `make api-contracts` from the root regenerates
the spec and these types together, and the CI `contracts` job fails on drift.

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
| `radix-ui` | accessible headless primitives backing Flag's `@flag/*` shadcn/ui components (`src/components/ui/`) |
| `class-variance-authority`, `clsx`, `tailwind-merge` | variant definitions (`cva`) and conflict-safe Tailwind v4 class merging (`src/lib/utils.ts`) for `@flag/*` |
| `lucide-react` | structural control chrome icons inside `@flag/*` registry primitives (`src/components/ui/`); product UI in `src/app/` uses Flag's `core-icons.svg` sprite |
| `cmdk` | combobox / command-menu primitive backing `@flag/command` (`src/components/ui/command.tsx`) |
| `sonner` | accessible toast primitive backing `@flag/sonner` (`src/components/ui/sonner.tsx`) |
| `tw-animate-css` | CSS keyframe utilities imported by `src/app/globals.css` for `@flag/*` overlays and popovers |

Code highlighting is in-housed (`src/app/chat/ui/highlight/`, see the root
`AGENTS.md`). Add a runtime dependency only with a row here and in the root
`AGENTS.md` with its reason.

## UI components and design

- **Base UI components come from Flag's `@flag/*` shadcn/ui registry**
  (`src/components/ui/*`, configured in `components.json` pointing `@flag` at
  `https://flag.elcanotek.com/r/{name}.json` and installed via
  `npx shadcn add @flag/<item>`). Login, No-Access, Settings, Operations Center
  (`/orchestrator`), Analytics, Chat controls/dialogs, and the Patrol Launchpad
  use these `@flag/*` primitives (`Button`, `Input`, `Textarea`, `NativeSelect`,
  `Switch`, `Checkbox`, `Badge`, `Alert`, `Card`, `Table`, `Tabs`, `Avatar`,
  `Kbd`, `Separator`, …), with overlay z-indices (`z-[1100]` dialogs/sheets,
  `z-[1300]` floating popovers/menus/selects/tooltips) aligned to fleet's modal
  stack. Build a base component by hand only when the registry lacks it, then
  upstream it to Flag.
- Design rules (tokens, contrast, typefaces): [`src/app/DESIGN.md`](src/app/DESIGN.md).
  `src/app/globals.css` imports `tw-animate-css` and the vendored
  `src/flag/shadcn/theme.css` bridge while keeping `fonts/fonts.css` imported
  from `src/main.tsx` (never `@import`ed inside CSS) and keeping `:root`'s raw
  `--color-primary` literal in `globals.css` for Go's bundle-theming contrast
  check (`internal/httpapi/theme_contrast_test.go`).
- **Vendored Flag files** — `src/app/fonts/` (whole dir),
  `public/icons/core-icons.svg`, `public/logos/fleet-mark.svg`,
  `src/flag/shadcn/theme.css` and `src/app/FLAG-LICENSE` — are listed with
  hashes in the root `flag.lock`.
  Never edit them: `scripts/flag-sync.sh check` (part of `make stack-check`)
  fails on any change. Update with `scripts/flag-sync.sh sync ../flag` from a
  clean sibling Flag checkout, then commit the files and `flag.lock` together.
  Every icon comes from Flag's sprite — use Flag's ids (`book`,
  `alert-triangle`, `tools`, …). `src/app/shared/ui/iconHref.ts` keeps a few
  aliases (`book-open`, `warning`, `wrench`) only for names that arrive as
  client-bundle data; `spriteCoverage.test.ts` rejects them in fleet's source.
  A glyph Flag lacks is requested from Flag, not drawn here.

## Tests

- vitest beside the code (`*.test.ts(x)`), jsdom. Mocks of
  `@/app/lib/navigation` stub only the hooks; `Link` lives in its own module.
- Playwright (`e2e/`): `mocked` (deterministic, the Go gateway alone with
  route-intercepted backends — includes the axe accessibility spec), `live`
  (real stack + sandbox, fake LLM), `canary`, `screenshots`. See
  [`e2e/README.md`](e2e/README.md). Never run `playwright install` in a
  sandboxed agent session; use the browsers the environment provides.
