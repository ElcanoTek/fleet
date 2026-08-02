import type { MetadataRoute } from "next";

import { DEFAULT_BACKGROUND_DARK, getServerBranding } from "@/app/lib/serverBranding";

// force-dynamic is LOAD-BEARING, and its absence was a live bug (#895).
//
// This route was written to resolve the bundle's palette at request time, and it
// never did once: Next statically prerendered it, so the fetch ran during
// `next build` — which scripts/update.sh performs in a staging dir with the
// backend down — and silently took the fallback on every deployment ever made.
// The Reklaim sandbox served `theme_color: "#1a0b1e"` (fleet purple) while
// /api/theme, on the same host, correctly served `--color-bg: #0A0908`.
//
// The failure mode is invisible without diffing two live endpoints, so the flag
// is asserted in serverBranding.test.ts. Do not remove it.
export const dynamic = "force-dynamic";

// Colour validation and the app-name fallback both live in serverBranding now,
// shared with layout.tsx's generateMetadata/generateViewport, so the manifest and
// the <meta> tags cannot disagree about what the deployment is called or coloured.
export default async function manifest(): Promise<MetadataRoute.Manifest> {
  const b = await getServerBranding();

  // A manifest has ONE color pair while the palette has two modes; take the dark
  // one, the mode fleet defaults to. `background_color` is the installed-app
  // splash and is manifest-only. `theme_color` here is largely advisory —
  // <meta name="theme-color"> from layout.tsx wins in Chrome — but it must agree
  // with it rather than contradict it.
  const splash = b.backgroundDark ?? DEFAULT_BACKGROUND_DARK;

  return {
    name: b.appName,
    short_name: b.appName,
    start_url: "/",
    display: "standalone",
    background_color: splash,
    theme_color: splash,
    icons: [
      // The bundle's mark, so an installed app does not land on the home screen
      // wearing fleet's icon under the deployment's own name (#895). No exact
      // `sizes` claim: the route serves whatever single file the bundle declared,
      // and asserting a resolution fleet has not verified is a lie the OS acts
      // on. "any" lets the browser scale it, which suits the square marks bundles
      // actually ship (Reklaim's is a 256x256 tile). The route redirects to
      // fleet's own mark when the bundle declares no logo, so this is never dead.
      {
        src: "/api/brand/logo",
        sizes: "any",
        purpose: "any",
      },
      // Maskable stays fleet's asset. A maskable icon must keep its artwork
      // inside a safe zone with ~20% bleed on every side, which an arbitrary
      // bundle file does not satisfy — Android would crop a full-bleed mark.
      // A correctly-padded generic icon beats a cropped client one. A bundle
      // supplying its own maskable rendition is the follow-up noted in #895
      // (optional `branding.pwa_icons`).
      {
        src: "/app-icons/maskable-icon-512.png",
        sizes: "512x512",
        type: "image/png",
        purpose: "maskable",
      },
    ],
  };
}
