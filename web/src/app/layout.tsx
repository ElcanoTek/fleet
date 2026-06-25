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
  icons: {
    icon: [
      { url: "/favicon.ico", sizes: "any" },
      { url: "/icon.png", type: "image/png", sizes: "512x512" },
      { url: "/app-icons/favicon-32.png", type: "image/png", sizes: "32x32" },
      { url: "/app-icons/favicon-16.png", type: "image/png", sizes: "16x16" },
    ],
    apple: [{ url: "/apple-icon.png", type: "image/png", sizes: "180x180" }],
    shortcut: "/favicon.ico",
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
        url: "/icon.png",
        alt: APP_NAME,
      },
    ],
  },
  twitter: {
    card: "summary_large_image",
    title: SHARE_TITLE,
    description: SHARE_DESCRIPTION,
    images: ["/icon.png"],
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
  themeColor: [
    { media: "(prefers-color-scheme: light)", color: "#f6f4f9" },
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
