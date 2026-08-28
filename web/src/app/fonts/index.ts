// Self-hosted IBM Plex, loaded through next/font/local.
//
// Both faces are SIL Open Font License 1.1 (© 2017 IBM Corp., Reserved Font
// Name "Plex"), which is what makes them shippable in this MIT-licensed repo.
// ./OFL.txt is the licence text and MUST stay next to the .woff2 files — the OFL
// requires the notice to travel with the font, and deleting it would make this
// repo's distribution non-compliant.
//
// These are IBM's COMPLETE builds (895 codepoints for Sans, 983 for Mono:
// Latin-1, Latin Extended-A/B, Greek, Cyrillic). Do not swap in a narrower
// subset to save bytes — a missing glyph does not error, it silently falls back
// to a system font for that one character, so a name renders in two typefaces
// mid-word. The flag design system carries the same rule and the check for it
// (design-system/FONTS.md).
//
// Weights are limited to the two the design system actually uses: 400 for
// body/UI, 700 for headings. next/font/local self-hosts and preloads these, so
// there is no request to a font CDN at runtime.
import localFont from "next/font/local";

export const plexSans = localFont({
  src: [
    { path: "./IBMPlexSans-400.woff2", weight: "400", style: "normal" },
    { path: "./IBMPlexSans-700.woff2", weight: "700", style: "normal" },
  ],
  variable: "--font-plex-sans",
  display: "swap",
  fallback: ["Segoe UI", "system-ui", "-apple-system", "sans-serif"],
});

export const plexMono = localFont({
  src: [
    { path: "./IBMPlexMono-400.woff2", weight: "400", style: "normal" },
    { path: "./IBMPlexMono-700.woff2", weight: "700", style: "normal" },
  ],
  variable: "--font-plex-mono",
  display: "swap",
  fallback: ["SF Mono", "Consolas", "ui-monospace", "monospace"],
});
