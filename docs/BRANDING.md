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
  share_image: "assets/acme-share.png"
  support:
    label: "Contact Support"
    email: "support@example.com"
    title: "Direct Support"
    subject: "Workspace Feedback / Support"
    message: |-
      Have a question or want something tuned for your workflow? Reach out anytime.
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

The web tier reads it in-process through `internal/webui/brand.go` (cached
for 60s, and it **never fails a page** — every failure path returns the same
defaults `clientconfig.applyBrandingDefaults` gives a sparse manifest). The
page head Go renders into `index.html` (`<title>`, `og:*`, `twitter:*`,
`theme-color`), the bootstrap JSON the login card reads, and
`/manifest.webmanifest` all resolve from that one helper, so the tab, the login
page, the share card, and the installed app cannot disagree about what the
deployment is called ([ADR-0091](adr/0091-go-serves-the-browser-tier.md)).

Every field on `/brand/meta` is public **by construction** — the app name is in
the browser tab, the login copy is printed on the pre-auth page, the share
strings go into OG tags anonymous scrapers read, and the backgrounds are already
served to that audience by `/theme.css`. It deliberately does **not** carry the
rest of `/client-config`: the empty-state catalog is workspace content, not
public identity.

Everything above is resolved per request (Go renders `index.html` on every
page load), so there is no build-time branding to go stale: the old Next.js
tier's two traps — a statically prerendered manifest and metadata baked into
prerendered HTML — cannot recur, and `NEXT_PUBLIC_APP_NAME` is no longer read.

`/api/theme`, `/api/brand/logo`, and `/api/brand/share-image` are in the
web tier's public-path set (`internal/webui/gateway.go`) because something outside a session fetches them.
For the share image that is a hard requirement rather than a convenience:
link-unfurl scrapers are anonymous, so an `og:image` behind the session gate
renders no preview at all. `/brand/meta` has no browser route at all: it
is read only server-side, by `internal/webui/brand.go`, so there is nothing to expose
publicly and the surface stays smaller.

The bar for adding a route to this class is that its response is public **by
construction** — already visible to anyone who can load the login page or scrape
a shared link — not merely that it looks non-sensitive. `/client-config` stays
member-gated because it also carries workspace content.

### Support contact (`branding.support`)

A bundle may optionally declare `branding.support` to render a prominent support
or escalation pill button in the top bar across every authenticated surface
(Chat, Operations Center, Settings, and Guides). Clicking the button opens a
dialog with the configured message, a copyable email address, and a `mailto:`
action.

| Field | Required | Description |
|---|---|---|
| `label` | Yes | Button label shown in the top bar (and fallback dialog title when `title` is omitted). |
| `email` | Yes | Plain email address (`local@domain`); validated at bundle load (`fleet validate-config`) and rejected if it contains a display name or URL/query characters. |
| `title` | No | Heading shown at the top of the support dialog (defaults to `label`). |
| `message` | No | Body copy shown inside the dialog; blank lines split paragraphs. |
| `subject` | No | Default subject line URL-encoded onto the `mailto:` link. |

`branding.support` is served **only** on the member-gated `/client-config`
route (never on `/brand/meta`), so operator or escalation contact details are
not exposed to unauthenticated visitors or link-unfurl scrapers. When omitted,
no support button is rendered.

## Applying a change

Bundle branding is read when the bundle loads, so a re-theme takes a restart
(`fleet restart`); downstream caching is 5 minutes. `fleet validate-config`
reports a bad `logo` or `share_image` path before you restart into it.

Unfurl scrapers cache aggressively on their own side, so a changed `share_image`
can take a while to appear in Slack even after a restart — Slack's
`/slackdebug` unfurl tools and `cards-dev.twitter.com/validator` force a refetch.
