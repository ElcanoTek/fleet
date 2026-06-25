import { NextResponse } from "next/server";
import { getChatServerBase, getSharedToken } from "@/app/lib/chatServer";

export const runtime = "nodejs";

// GET /api/theme — PUBLIC (no session) proxy for chat-server's /theme.css, the
// deployment's brand palette rendered as a stylesheet. It is intentionally
// un-gated: the login page (pre-auth) links it to paint in the client's colors,
// and the palette is non-secret + deployment-wide. It ALWAYS returns valid CSS
// with a 200 — empty on any backend trouble — so a linked <link rel=stylesheet>
// can never block or break the shell's first paint.
export async function GET() {
  let css = "";
  try {
    const upstream = await fetch(`${getChatServerBase()}/theme.css`, {
      headers: { "X-Chat-Server-Token": getSharedToken() },
      cache: "no-store",
    });
    if (upstream.ok) {
      css = await upstream.text();
    }
  } catch {
    css = "";
  }
  return new NextResponse(css, {
    status: 200,
    headers: {
      "Content-Type": "text/css; charset=utf-8",
      "Cache-Control": "public, max-age=300",
    },
  });
}
