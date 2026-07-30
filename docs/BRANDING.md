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
reach the browser two ways: through the member-gated `/client-config` for
in-app, post-login surfaces, and through the token-gated, identity-less
`/brand/meta` for everything that renders **without a session**.

`/brand/meta` exists because the surfaces that most define a deployment's
identity are all pre-auth or account-less, and none of them can reach
`/client-config`:

| Surface | Why it can't use `/client-config` |
|---|---|
| The login card's title + tagline | Renders before a session exists |
| `<title>`, `og:*`, `twitter:*` | Resolved server-side with no user; unfurl scrapers are anonymous |
| `<meta name="theme-color">`, the PWA manifest | Same, and neither can read a CSS variable |

The web reads it once per request through `web/src/app/lib/serverBranding.ts`
(memoized in-process for 60s, 2s fetch timeout, and it **never throws** — every
failure path returns the same defaults `clientconfig.applyBrandingDefaults`
gives a sparse manifest). `login/page.tsx`, `layout.tsx`'s `generateMetadata` /
`generateViewport`, and `manifest.ts` all resolve from that one helper, so the
tab, the login page, the share card, and the installed app cannot disagree about
what the deployment is called.

Every field on `/brand/meta` is public **by construction** — the app name is in
the browser tab, the login copy is printed on the pre-auth page, the share
strings go into OG tags anonymous scrapers read, and the backgrounds are already
served to that audience by `/theme.css`. It deliberately does **not** carry the
rest of `/client-config`: the empty-state catalog is workspace content, not
public identity.

`NEXT_PUBLIC_APP_NAME` is now only a **fallback**, used when the backend is
unreachable. The bundle wins whenever it can be read, so setting the env var is
no longer required to get the deployment's own name in the tab.

> **Two build-time traps, both of which shipped as live bugs.** Anything that
> reads the bundle must render at **request** time. `manifest.ts` was written to
> resolve the palette per request and never did once — its route was statically
> prerendered, so the fetch ran during `next build` in a staging dir with the
> backend down, and every deployment silently served the fallback. Separately, a
> root-layout `generateMetadata` is not sufficient on its own: routes that don't
> otherwise opt out get prerendered with the metadata **baked into their HTML**.
> Hence `export const dynamic = "force-dynamic"` in both `manifest.ts` and
> `layout.tsx` (where it covers the whole subtree). Both flags are asserted in
> `serverBranding.test.ts`, because the failure mode is silent — the only symptom
> is two live endpoints disagreeing. Do not remove them.

The **tab icon** is bundle-driven: `layout.tsx` declares `metadata.icons`
pointing at `/api/brand/logo`. That overrides App Router's `icon.svg` and
`apple-icon.png` file conventions — but **not** `app/favicon.ico`, which Next
special-cases and emits unconditionally. That file used to sit alongside the
declaration and, being the only candidate carrying `sizes` and `type`, won the
tab strip, so a white-labeled deployment showed fleet's purple mark beside its
own name. It has been deleted; **do not reintroduce it**. Unbranded deployments
lose nothing, because `/api/brand/logo` 307-redirects to `/logos/fleet-mark.svg`
on every failure path.

The **installed-app icon** and **splash color** are bundle-driven too.
`manifest.ts` points its `any`-purpose icon at `/api/brand/logo` with
`sizes: "any"` — no exact size is claimed, because the route serves whatever
single file the bundle declared and asserting a resolution fleet hasn't verified
is a lie the OS acts on. The **maskable** icon stays fleet's own asset: a
maskable icon must keep its artwork inside a safe zone with ~20% bleed, which an
arbitrary bundle file does not satisfy, and Android would crop a full-bleed
mark. A bundle supplying its own maskable rendition (`branding.pwa_icons`) is a
deliberate follow-up.

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

### What a palette now reaches

Two things used to sit outside `colors` and could not be themed, so a bundle that
set every token still rendered fleet's own colors on its most visible surfaces.
Both are fixed:

- **Gradients.** `--gradient-bg` (painted on `<body>`), `--sidebar-surface` (the
  rail), the surface/panel/card/composer gradients, and `--gradient-action-primary`
  were literal fleet-purple. They are now derived from `--color-bg`,
  `--color-surface-1/-2`, `--color-primary`, and `--color-secondary` via
  `color-mix()`. The percentages were fitted against the previous literals for
  fleet's own palette, so the stock look is unchanged (every stop within ΔRGB ≤ 6).
  **Do not reintroduce a literal color in a gradient** — a bundle cannot override
  it and no test will catch it.
- **Light-mode agent links** and the light **usage-bar** hue were literal too
  (`#5f5f97` / `#3f3f7a` / `#6363b8`); they now derive from `--color-accent` and
  `--color-primary`.

### `on_primary`

The readable foreground **on** a primary fill — buttons, active segments, the
switch knob, the user avatar, and the `--gradient-action-primary` surface.

It has to be declared rather than derived, and a bundle with a **light** primary
must set it. Every one of those rules previously hardcoded white, so a
yellow-primary bundle rendered white-on-yellow at **1.33:1**. Declaring
near-black instead takes the same surface to 14.87:1.

Light mode additionally takes `--color-primary-hover` as the deep end of the
action gradient, on the reasoning that "the deeper primary" is what that token
already means there — so a light-primary bundle supplies a deeper *shade of its
own hue* instead of having fleet mix its brand color toward black.

### `colors`

Per-mode overrides of the CSS custom properties `globals.css` defines, rendered
by `/theme.css` as a render-blocking stylesheet linked from the root layout — so
the shell, **including the pre-auth login page**, paints in the deployment's
palette with no flash.

| Group | Tokens |
|---|---|
| Core | `primary`, `primary_hover`, `on_primary`, `secondary`, `accent` |
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

## Trust class of the three brand routes

`/theme.css`, `/brand/logo`, and `/brand/meta` are all **token-gated but
identity-less**. A palette, a mark, and a deployment's name are deployment-wide
and non-secret, and the login shell has to render all three before a session
exists. Each degrades quietly — empty CSS, a redirect to fleet's mark, or the
generic defaults — so none can block or break first paint if the backend is
unreachable.

`/api/theme` and `/api/brand/logo` are in the middleware's public-path set,
because the **browser** fetches them. `/brand/meta` has no Next proxy at all: it
is read only server-side, by `serverBranding.ts`, so there is nothing to expose
publicly and the surface stays smaller.

The bar for adding a route to this class is that its response is public **by
construction** — already visible to anyone who can load the login page or scrape
a shared link — not merely that it looks non-sensitive. `/client-config` stays
member-gated because it also carries workspace content.

## Applying a change

Bundle branding is read when the bundle loads, so a re-theme takes a restart
(`fleet restart`); downstream caching is 5 minutes. `fleet validate-config`
reports a bad `logo` path before you restart into it.
