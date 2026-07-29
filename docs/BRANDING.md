# White-labeling fleet from a bundle

Everything a deployment shows about itself — name, login copy, palette, and the
mark in the navigation rail — comes from the client-config bundle's
`manifest.yaml`, not from fleet's source. This page is the complete list of what
is bundle-driven, what is not, and why.

## `branding:` reference

```yaml
branding:
  app_name: "Acme"
  login_title: "Welcome aboard."
  login_tagline: "Sign in to your workspace and pick up where you left off."
  share_title: "Acme — your team's AI workspace"
  share_description: "Persistent multi-turn conversations with real tool use."
  logo: "assets/acme-mark.svg"
  colors:
    dark:
      primary: "#e6007e"
      border_strong: "rgba(230, 0, 126, 0.55)"
      rail_hover: "rgba(255, 255, 255, 0.07)"
    light:
      primary: "#a4005a"
```

A sparse block is fine: every field falls back to the generic value in
`clientconfig.applyBrandingDefaults`, which is the same source of truth the
no-bundle UI uses, so a partially branded bundle can never drift from the
default experience.

### Strings

`app_name`, `login_title`, `login_tagline`, `share_title`, `share_description`
reach the browser through the member-gated `/client-config`.

The **browser tab title** and the **PWA name** are a separate knob:
`NEXT_PUBLIC_APP_NAME` in the web env file, read at build time by
`layout.tsx` / `manifest.ts`. They are not bundle-driven, because Next resolves
static metadata and the file-convention icons (`icon.svg`, `favicon.ico`,
`apple-icon.png`) when the app is built, not per request. Set the env var
alongside the bundle so the tab matches the shell.

### `logo`

A bundle-relative path to `.svg`, `.png`, `.webp`, `.jpg`, `.jpeg`, or `.ico`.

fleet copies nothing into `web/public`. The backend serves the file from the
bundle at `/brand/logo`, proxied to the browser as `/api/brand/logo`, so
re-theming is a bundle change plus a restart — never a web rebuild. Omit the
field and the rail renders fleet's own mark.

The path is resolved and containment-checked **at load** (`resolveBrandLogo`),
so a bad value fails at startup rather than silently: it must be lexically local
(`filepath.IsLocal` — no absolute path, no `..`), must still resolve inside the
bundle after symlink resolution, must be a regular file, and must carry an
extension fleet knows a content type for. Beyond that the route caps the file at
2 MiB and, since bundle content is operator-authored, hardens delivery
defensively rather than as a trust boundary: `X-Content-Type-Options: nosniff`
plus `Content-Security-Policy: default-src 'none'; sandbox`, so an SVG carrying
`<script>` executes nothing even if someone opens the asset URL directly.

`/client-config` advertises `logo_url` **only** when a file actually backed the
field at load, so the web never points an `<img>` at a route that 404s.

The rail renders the mark with `next/image` marked **`unoptimized`**, and that is
load-bearing rather than a perf opt-out — don't "tidy" it away. `next/image`
skips Next's image optimizer only when the `src` path literally ends in `.svg`
(the "special case to make svg serve as-is" in `get-img-props.js`). A bundle mark
arrives as `/api/brand/logo`, which has no extension, so without `unoptimized`
it was rewritten to `/_next/image?url=…` — and that endpoint **rejects**
`image/svg+xml` unless `images.dangerouslyAllowSVG` is set, which fleet's
`next.config.ts` deliberately does not set. The result was a broken mark on every
page for any bundle whose `logo` was an SVG (which is the format this doc's own
example uses). Optimizing a 28px mark buys nothing, so serving it as-is is also
the right trade on its own terms. `web/src/app/shared/ui/NavRail.test.tsx` pins
the behavior by asserting the rendered `src` is the raw path and that no `srcset`
is generated.

### `colors`

Per-mode overrides of the CSS custom properties `globals.css` defines, rendered
by `/theme.css` as a render-blocking stylesheet linked from the root layout — so
the shell, **including the pre-auth login page**, paints in the deployment's
palette with no flash.

| Group | Tokens |
|---|---|
| Core | `primary`, `primary_hover`, `secondary`, `accent` |
| Surfaces | `background`, `surface_1`, `surface_2` |
| Text | `text_primary`, `text_secondary`, `text_muted`, `text_disabled` |
| Structure | `border`, `border_strong`, `border_subtle` |
| Scrims | `overlay_soft`, `overlay_strong` |
| Nav rail | `rail_hover`, `rail_active` |

Set the structure, scrim, and rail tokens alongside the core ones. `globals.css`
hand-tints its defaults from fleet's own primary hue rather than deriving them,
so a bundle that overrides only `primary` leaves fleet-tinted emphasis borders
and rail rows beside its own palette.

Values may be hex or `rgb()`/`rgba()`/`hsl()`/`hsla()`. Anything else — and any
token name fleet does not theme — is dropped at render time, and that one token
falls back to its default; a typo cannot break the stylesheet. Because
`BrandColors` is a map, listing a token fleet does not theme yet will not make
the strict manifest decoder reject the bundle.

Light and dark are independent: theme one and the other keeps fleet's defaults.

**Semantic status colors are deliberately not themable** — `--color-success`,
`--color-danger`, `--color-warning` and their borders. They encode meaning
rather than brand (a failed tool call must read as failure in every deployment),
and several are derived with `color-mix()` from the base hue, so a partial
override would desynchronize a swatch from its own border.

## Trust class of the two asset routes

`/theme.css` and `/brand/logo` are both **token-gated but identity-less**, and
their Next proxies (`/api/theme`, `/api/brand/logo`) are in the middleware's
public-path set. A palette and a mark are deployment-wide and non-secret, and
the login shell has to render both before a session exists. Both degrade
quietly — empty CSS, or a 404 that falls back to fleet's mark — so neither can
block or break first paint if the backend is unreachable.

## Applying a change

Bundle branding is read when the bundle loads, so a re-theme takes a restart
(`fleet restart`); downstream caching is 5 minutes. `fleet validate-config`
reports a bad `logo` path before you restart into it.
