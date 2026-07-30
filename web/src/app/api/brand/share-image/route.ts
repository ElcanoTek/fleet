import { NextResponse } from "next/server";

import { getChatServerBase, getSharedToken } from "@/app/lib/chatServer";

export const runtime = "nodejs";
// Resolved per request from the bundle, never baked at build time — same rule as
// every other bundle-reading surface (see lib/serverBranding.ts).
export const dynamic = "force-dynamic";

// Content types this proxy will pass through, mirroring the allowlist
// clientconfig validates the manifest path against. The upstream already
// restricts what it serves; re-checking here means a future upstream change
// cannot turn this route into a pass-through for markup or scripts.
//
// SVG is deliberately absent, unlike /api/brand/logo. No unfurl scraper renders
// an SVG og:image, so allowing one would only widen what this route can serve for
// no benefit.
const ALLOWED_TYPES = new Set(["image/png", "image/webp", "image/jpeg"]);

// GET /api/brand/share-image — PUBLIC (no session) proxy for chat-server's
// /brand/share-image, the og:image / twitter:image declared by the bundle's
// branding.share_image.
//
// Public is not a convenience here, it is a requirement: link-unfurl scrapers
// (Slack, iMessage, Discord, Teams, LinkedIn) are anonymous, so an og:image
// behind a session gate renders no preview at all.
//
// When the bundle declares no share image — or the upstream is unreachable — this
// REDIRECTS to fleet's own neutral card rather than 404ing, so the og:image URL
// is always a valid image. Mirrors /api/brand/logo's fallback for the same
// reason: a dead og:image is worse than a generic one.
function fleetCard(request: Request): NextResponse {
  return NextResponse.redirect(new URL("/share.png", request.url), 307);
}

export async function GET(request: Request) {
  try {
    const upstream = await fetch(`${getChatServerBase()}/brand/share-image`, {
      headers: { "X-Chat-Server-Token": getSharedToken() },
      cache: "no-store",
    });
    if (!upstream.ok) {
      return fleetCard(request);
    }
    const type = (upstream.headers.get("content-type") ?? "").split(";")[0].trim().toLowerCase();
    if (!ALLOWED_TYPES.has(type)) {
      return fleetCard(request);
    }
    return new NextResponse(await upstream.arrayBuffer(), {
      status: 200,
      headers: {
        "Content-Type": type,
        "X-Content-Type-Options": "nosniff",
        "Content-Security-Policy": "default-src 'none'; sandbox",
        // Longer than the 5-minute brand-asset TTL: unfurl scrapers cache
        // aggressively on their own side anyway, and a share card changes only
        // when the bundle does (which needs a restart regardless).
        "Cache-Control": "public, max-age=300",
      },
    });
  } catch {
    return fleetCard(request);
  }
}
