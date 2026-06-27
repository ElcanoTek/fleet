import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";

export const runtime = "nodejs";

// GET /api/search?q=…&type=…&limit=…&offset=… — proxies full-text search (#308)
// to the chat server, forwarding the query string verbatim. Read-only, so no
// CSRF origin check (matches GET /api/conversations).
export async function GET(request: NextRequest) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const qs = request.nextUrl.search; // "?q=…" (already URL-encoded) or ""
  const { upstream, error } = await chatServerProxy(session.email, `/search${qs}`, { method: "GET" });
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
