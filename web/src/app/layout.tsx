import type { Metadata, Viewport } from "next";
import Script from "next/script";
import { GeistSans } from "geist/font/sans";
import { GeistMono } from "geist/font/mono";
import "./globals.css";

// Branding here is static/SSR metadata that scrapers read to unfurl shared
// links, so it can't fetch the member-gated /client-config. Instead it reads
// build-time NEXT_PUBLIC_* env vars (with neutral, client-agnostic defaults) to
// stay white-labellable without a runtime fetch. Per-request branding (the tab
// title, sidebar) is overridden client-side from /api/client-config.
const APP_NAME = process.env.NEXT_PUBLIC_APP_NAME?.trim() || "Fleet";

// metadataBase resolves relative URLs (icons, OG images) against this origin
// when scrapers unfurl shared links. Configurable via env so each deploy points
// at its own public hostname; the default is a neutral placeholder.
const PUBLIC_ORIGIN =
  process.env.NEXT_PUBLIC_PUBLIC_ORIGIN?.trim() || "https://chat.example.com";

const SHARE_TITLE = `${APP_NAME} — your team's AI workspace`;
const SHARE_DESCRIPTION =
  "Persistent multi-turn conversations with real tool use across files, data, and the web.";

export const metadata: Metadata = {
  metadataBase: new URL(PUBLIC_ORIGIN),
  // Default tab title. The chat experience overrides this with
  // "{conversation title} — {app name}" once a conversation is active.
  title: APP_NAME,
  description: SHARE_DESCRIPTION,
  applicationName: APP_NAME,
  authors: [{ name: APP_NAME }],
  manifest: "/manifest.webmanifest",
  // The tab icon comes from the client-config bundle's branding.logo, served at
  // /api/brand/logo. Declaring it here deliberately overrides App Router's
  // file-convention icons (icon.svg / apple-icon.png in this directory), which
  // are build-time assets a bundle cannot reach — so without this every
  // deployment wore fleet's mark in the tab beside its own name.
  //
  // No build-time knowledge of the bundle is needed (this file must not fetch
  // member-gated config): the browser resolves the path at request time, and
  // that route redirects to fleet's own mark when the bundle declares no logo,
  // so the link is never dead.
  icons: {
    icon: "/api/brand/logo",
    shortcut: "/api/brand/logo",
    apple: "/api/brand/logo",
  },
  appleWebApp: {
    capable: true,
    title: APP_NAME,
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
    siteName: APP_NAME,
    title: SHARE_TITLE,
    description: SHARE_DESCRIPTION,
    url: PUBLIC_ORIGIN,
    images: [
      {
        url: "/share.png",
        width: 1280,
        height: 640,
        alt: "Put your agents to work on infrastructure you own.",
      },
    ],
  },
  twitter: {
    card: "summary_large_image",
    title: SHARE_TITLE,
    description: SHARE_DESCRIPTION,
    images: ["/share.png"],
  },
};

export const viewport: Viewport = {
  width: "device-width",
  initialScale: 1,
  // Intentionally do NOT set maximumScale — pinch-to-zoom must stay
  // available for accessibility (WCAG 1.4.4). The input-focus auto-zoom
  // that plagues iOS Safari is suppressed instead by forcing a 16px
  // minimum font-size on inputs/textareas/selects in globals.css.
  viewportFit: "cover",
  // Browser-chrome tint. Meta tags can't read CSS custom properties, so these
  // literals must mirror --color-bg for each theme in globals.css — keep them
  // in sync when the background tokens change.
  themeColor: [
    { media: "(prefers-color-scheme: light)", color: "#f4f6fb" },
    { media: "(prefers-color-scheme: dark)", color: "#1a0b1e" },
  ],
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html
      lang="en"
      className={`${GeistSans.variable} ${GeistMono.variable} h-full antialiased`}
      suppressHydrationWarning
    >
      <head>
        <Script src="/scripts/theme.js" strategy="beforeInteractive" />
        {/* Brand palette from the client-config bundle (branding.colors),
            served by chat-server as a render-blocking stylesheet. Its
            html:root[data-theme=…] rules out-specify globals.css, so the shell —
            including the pre-auth login page — paints in the client's colors
            with no flash. Empty (a no-op) when the bundle declares no colors.
            Deliberately a runtime <link>, not build-bundled CSS: the palette is
            resolved from the manifest at request time, which next/font-style
            CSS handling can't express — hence the rule suppression. */}
        {/* eslint-disable-next-line @next/next/no-css-tags */}
        <link rel="stylesheet" href="/api/theme" />
      </head>
      <body className="min-h-full flex flex-col">{children}</body>
    </html>
  );
}
