import type { Metadata, Viewport } from "next";
import {
  DEFAULT_BACKGROUND_DARK,
  DEFAULT_BACKGROUND_LIGHT,
  getServerBranding,
} from "@/app/lib/serverBranding";
import "./globals.css";

// force-dynamic is LOAD-BEARING for white-labeling, and it applies to this
// layout's whole subtree — i.e. every page.
//
// Without it, Next statically prerenders the routes that don't otherwise opt out
// (/settings/*, /no-access, /) and BAKES generateMetadata's output into their
// HTML at build time. The build runs with no backend reachable (scripts/update.sh
// builds in a staging dir), so those pages shipped `<title>Fleet</title>`
// hardcoded into the artifact — verified by grepping .next/server/app/*.html.
// /login, /chat and /orchestrator were already dynamic and would have been
// correct, so the bug would have shown up as *some* tabs saying "Fleet" and
// others saying the deployment's name, which is a miserable thing to debug.
//
// Baking is wrong on principle too: one build artifact must be able to serve any
// bundle, since a re-theme is documented as "a bundle change plus a restart —
// never a web rebuild" (docs/BRANDING.md).
//
// The cost is nil in practice. proxy.ts stamps `Cache-Control: no-store` on
// every response and the middleware runs on every request, so a prebuilt shell
// was never actually being served from cache.
export const dynamic = "force-dynamic";

// Branding is resolved PER REQUEST from the client-config bundle, server-side.
//
// It used to read build-time NEXT_PUBLIC_* env vars, on the reasoning that this
// file must not fetch the member-gated /client-config. The constraint is real;
// the conclusion was wrong. chat-server's token-gated, identity-less /brand/meta
// serves the same strings without a session — the trust class /theme.css and
// /brand/logo already established for exactly this problem.
//
// The old shape left `branding.share_title` / `share_description` read by ZERO
// components (#894) and made the tab title depend on an env var nothing in the
// deploy path set from the bundle, so a fully branded deployment introduced
// itself as "Fleet" in its tab, its OG card, and its PWA name.
//
// Consequence to know about: a root-layout `generateMetadata` makes every route
// server-rendered on demand rather than statically prerendered. That is the
// right trade here — `proxy.ts` already stamps `Cache-Control: no-store` on
// every response and the middleware runs on every request, so a prebuilt shell
// bought nothing, while a wrong app name is on every page and every shared link.
async function branding() {
  return getServerBranding();
}

// metadataBase resolves relative URLs (icons, OG images) against this origin
// when scrapers unfurl shared links. Configurable via env so each deploy points
// at its own public hostname; the default is a neutral placeholder.
//
// Deliberately still env-driven, not bundle-driven: this is the deployment's
// public hostname, which is a property of the host, not of the client whose
// branding it wears. One bundle can be deployed at more than one origin.
const PUBLIC_ORIGIN =
  process.env.NEXT_PUBLIC_PUBLIC_ORIGIN?.trim() || "https://chat.example.com";

export async function generateMetadata(): Promise<Metadata> {
  const b = await branding();
  const shareTitle = b.shareTitle || b.appName;
  return {
    metadataBase: new URL(PUBLIC_ORIGIN),
    // Default tab title. The chat experience overrides this with
    // "{conversation title} — {app name}" once a conversation is active.
    title: b.appName,
    description: b.shareDescription,
    applicationName: b.appName,
    authors: [{ name: b.appName }],
    manifest: "/manifest.webmanifest",
    // The tab icon comes from the client-config bundle's branding.logo, served
    // at /api/brand/logo. Declaring it here overrides App Router's
    // file-convention icons — but note it can only override `icon.svg` and
    // `apple-icon.png`, NOT `app/favicon.ico`, which Next special-cases and
    // emits unconditionally. That file used to sit alongside this declaration
    // and, being the only candidate carrying `sizes` and `type`, won the tab
    // strip — so a white-labeled deployment showed fleet's purple mark next to
    // its own name (#891). It has been deleted; do not reintroduce it.
    //
    // No build-time knowledge of the bundle is needed: the browser resolves the
    // path at request time, and that route redirects to fleet's own mark when
    // the bundle declares no logo, so the link is never dead.
    icons: {
      icon: "/api/brand/logo",
      shortcut: "/api/brand/logo",
      apple: "/api/brand/logo",
    },
    appleWebApp: {
      capable: true,
      title: b.appName,
      statusBarStyle: "black-translucent",
    },
    // Internal tool — don't show up in Google. Slack / iMessage / Discord
    // unfurl scrapers ignore robots and still pull openGraph below, so the
    // share experience stays good.
    robots: {
      index: false,
      follow: false,
    },
    openGraph: {
      type: "website",
      siteName: b.appName,
      title: shareTitle,
      description: b.shareDescription,
      url: PUBLIC_ORIGIN,
      images: [shareImage(b.shareImageUrl, shareTitle)],
    },
    twitter: {
      card: "summary_large_image",
      title: shareTitle,
      description: b.shareDescription,
      images: [b.shareImageUrl ?? FLEET_SHARE_CARD],
    },
  };
}

// fleet's own neutral share card, used when the bundle declares no share_image.
const FLEET_SHARE_CARD = "/share.png";

// shareImage builds the og:image entry.
//
// The image itself used to be a hardcoded /share.png containing Elcano's logo and
// wordmark, with fleet's marketing headline as its alt text — served as the
// og:image for EVERY deployment (#893). Pasting a link to a white-labeled instance
// into Slack, iMessage, Discord or Teams unfurled with another company's brand,
// from the client's own domain, so nothing looked amiss to the unfurler.
//
// Dimensions are declared ONLY for fleet's own card, whose size we know. fleet
// does not decode a bundle's image, so it cannot honestly state width/height for
// an arbitrary asset — and a wrong declared size makes scrapers render a
// distorted or letterboxed preview. Omitting the tags makes them fetch and
// measure, which is correct rather than merely safe.
function shareImage(bundleUrl: string | null, alt: string) {
  if (bundleUrl) return { url: bundleUrl, alt };
  return { url: FLEET_SHARE_CARD, width: 1280, height: 640, alt };
}

// The browser-chrome tint (mobile address bar, PWA titlebar). A <meta> tag cannot
// read a CSS custom property, so this used to be a pair of fleet-purple literals
// with a comment asking future editors to keep them in sync with --color-bg by
// hand — and every white-labeled deployment therefore had purple chrome above its
// own palette (#895).
//
// It resolves from the bundle's `background` token per mode instead, falling back
// to those same literals. Note that <meta name="theme-color"> takes precedence
// over the PWA manifest's theme_color in Chrome, so this — not manifest.ts — is
// what actually paints the chrome.
export async function generateViewport(): Promise<Viewport> {
  const b = await branding();
  return {
    width: "device-width",
    initialScale: 1,
    // Intentionally do NOT set maximumScale — pinch-to-zoom must stay
    // available for accessibility (WCAG 1.4.4). The input-focus auto-zoom
    // that plagues iOS Safari is suppressed instead by forcing a 16px
    // minimum font-size on inputs/textareas/selects in globals.css.
    viewportFit: "cover",
    themeColor: [
      {
        media: "(prefers-color-scheme: light)",
        color: b.backgroundLight ?? DEFAULT_BACKGROUND_LIGHT,
      },
      {
        media: "(prefers-color-scheme: dark)",
        color: b.backgroundDark ?? DEFAULT_BACKGROUND_DARK,
      },
    ],
  };
}

// themeInitScript stamps data-theme on <html> BEFORE first paint. Both the
// globals.css light palette AND the bundle's brand palette (/api/theme, whose
// rules are html:root[data-theme=…]) key off that attribute, so until it
// exists the browser paints fleet's default :root colors.
//
// It MUST be an inline synchronous <script>. It used to be
// <Script src="/scripts/theme.js" strategy="beforeInteractive" />, but the App
// Router doesn't emit beforeInteractive scripts as parser-blocking tags — it
// queues them through the framework bootstrap (self.__next_s), which runs
// after streaming has started painting. Result: every hard refresh on a
// themed deployment flashed fleet's own palette for a beat before the brand
// rules could match (reported by a Reklaim user). An inline script in <head>
// executes during parse, before any body content exists to paint, closing the
// gap for both the light/dark choice and the brand palette.
//
// Keep the storage key + fallback in sync with useTheme.ts, which owns the
// same contract after hydration (THEME_STORAGE_KEY there; it can't be imported
// here because this is a server component and that module is client-only).
const themeInitScript = `(() => {
  const storageKey = "chat-theme-preference";
  const root = document.documentElement;
  try {
    const stored = window.localStorage.getItem(storageKey);
    const theme = stored === "light" || stored === "dark"
      ? stored
      : window.matchMedia("(prefers-color-scheme: dark)").matches
        ? "dark"
        : "light";
    root.setAttribute("data-theme", theme);
  } catch {
    root.setAttribute("data-theme", "dark");
  }
})();`;

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html
      lang="en"
      className="h-full antialiased"
      suppressHydrationWarning
    >
      <head>
        {/* Runs before the parser reaches <body>: see themeInitScript above. */}
        <script dangerouslySetInnerHTML={{ __html: themeInitScript }} />
        {/* Brand palette from the client-config bundle (branding.colors),
            served by chat-server as a render-blocking stylesheet. Its
            html:root[data-theme=…] rules out-specify globals.css, so the shell —
            including the pre-auth login page — paints in the client's colors
            with no flash (the inline script above guarantees the attribute
            those rules match on is set before first paint). Empty (a no-op)
            when the bundle declares no colors. Deliberately a runtime <link>,
            not build-bundled CSS: the palette is resolved from the manifest at
            request time, which next/font-style CSS handling can't express —
            hence the rule suppression. */}
        {/* eslint-disable-next-line @next/next/no-css-tags */}
        <link rel="stylesheet" href="/api/theme" />
      </head>
      <body className="min-h-full flex flex-col">{children}</body>
    </html>
  );
}
