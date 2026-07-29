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
// 404 (not an error page) when the bundle declares no logo, so the caller falls
// back to fleet's own mark. The web only points an <img> here when
// /client-config advertised logo_url, so the 404 path is the unusual one.
export async function GET() {
  try {
    const upstream = await fetch(`${getChatServerBase()}/brand/logo`, {
      headers: { "X-Chat-Server-Token": getSharedToken() },
      cache: "no-store",
    });
    if (!upstream.ok) {
      return new NextResponse(null, { status: 404 });
    }
    const type = (upstream.headers.get("content-type") ?? "").split(";")[0].trim().toLowerCase();
    if (!ALLOWED_TYPES.has(type)) {
      return new NextResponse(null, { status: 404 });
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
    return new NextResponse(null, { status: 404 });
  }
}
