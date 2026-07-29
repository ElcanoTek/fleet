import { NextResponse } from "next/server";

import { getChatServerBase, getSharedToken } from "@/app/lib/chatServer";

export const runtime = "nodejs";

// Content types this proxy will pass through, mirroring the allowlist
// clientconfig validates the manifest path against. The upstream already
// restricts what it serves; re-checking here means a future upstream change
// cannot turn this route into a pass-through for markup or scripts.
const ALLOWED_TYPES = new Set([
  "image/svg+xml",
  "image/png",
  "image/webp",
  "image/jpeg",
  "image/x-icon",
]);

// GET /api/brand/logo — PUBLIC (no session) proxy for chat-server's
// /brand/logo, the mark declared by the client-config bundle's branding.logo.
// Public for the same reason /api/theme is: the pre-auth login shell renders
// brand assets, and a logo is non-secret and deployment-wide rather than
// user-scoped.
//
// When the bundle declares no logo (or the upstream is unreachable), this
// REDIRECTS to fleet's own mark rather than 404ing, so the URL is always a valid
// image. That matters because layout.tsx points the favicon here: a 404 favicon
// shows the browser's blank-page glyph, which is worse than fleet's mark. The
// rail is unaffected either way — it only requests this route when
// /client-config advertised logo_url, i.e. when the upstream will answer 200.
function fleetMark(request: Request): NextResponse {
  return NextResponse.redirect(new URL("/logos/fleet-mark.svg", request.url), 307);
}

export async function GET(request: Request) {
  try {
    const upstream = await fetch(`${getChatServerBase()}/brand/logo`, {
      headers: { "X-Chat-Server-Token": getSharedToken() },
      cache: "no-store",
    });
    if (!upstream.ok) {
      return fleetMark(request);
    }
    const type = (upstream.headers.get("content-type") ?? "").split(";")[0].trim().toLowerCase();
    if (!ALLOWED_TYPES.has(type)) {
      return fleetMark(request);
    }
    return new NextResponse(await upstream.arrayBuffer(), {
      status: 200,
      headers: {
        "Content-Type": type,
        // Preserve the upstream's hardening: an SVG mark is a document the
        // browser parses, and this route is directly reachable.
        "X-Content-Type-Options": "nosniff",
        "Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; sandbox",
        "Cache-Control": "public, max-age=300",
      },
    });
  } catch {
    return fleetMark(request);
  }
}
