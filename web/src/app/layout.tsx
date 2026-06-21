import type { Metadata, Viewport } from "next";
import Script from "next/script";
import { GeistSans } from "geist/font/sans";
import { GeistMono } from "geist/font/mono";
import "./globals.css";

// metadataBase resolves relative URLs (icons, OG images) against this
// origin when scrapers unfurl shared links. Hardcoded to the production
// hostname because dev/staging URLs are never the ones being shared in
// Slack / iMessage / etc.
const PUBLIC_ORIGIN = "https://chat.elcanotek.com";

const SHARE_TITLE = "Elcano Chat — your team's AI workspace";
const SHARE_DESCRIPTION =
  "Persistent multi-turn conversations with real tool use across email, files, and analytics. Built by Elcano for the way your team actually works.";

export const metadata: Metadata = {
  metadataBase: new URL(PUBLIC_ORIGIN),
  // Default tab title. The chat experience overrides this with
  // "{conversation title} — Elcano Chat" once a conversation is active.
  title: "Elcano Chat",
  description: SHARE_DESCRIPTION,
  applicationName: "Elcano Chat",
  authors: [{ name: "Elcano" }],
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
    siteName: "Elcano Chat",
    title: SHARE_TITLE,
    description: SHARE_DESCRIPTION,
    url: PUBLIC_ORIGIN,
    images: [
      {
        url: "/logos/elcano-mark-primary.svg",
        alt: "Elcano",
      },
    ],
  },
  twitter: {
    card: "summary_large_image",
    title: SHARE_TITLE,
    description: SHARE_DESCRIPTION,
    images: ["/logos/elcano-mark-primary.svg"],
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
      </head>
      <body className="min-h-full flex flex-col">{children}</body>
    </html>
  );
}
