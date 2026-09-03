import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ conversationId: string }> };

// The download dialog picks the artifact (?format=html|markdown|json) and how
// much of the run it carries (?include=conversation|full). Both are forwarded
// verbatim; the Go server owns their meaning and their defaults, and rejects
// nothing — an unknown value falls back to the readable default there.
const FORWARDED_PARAMS = ["format", "include"] as const;

export async function GET(request: NextRequest, context: RouteContext) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId } = await context.params;
  const forwarded = new URLSearchParams();
  for (const key of FORWARDED_PARAMS) {
    const value = request.nextUrl.searchParams.get(key);
    if (value) forwarded.set(key, value);
  }
  const query = forwarded.toString();
  const { upstream, error } = await chatServerProxy(
    session,
    `/conversations/${encodeURIComponent(conversationId)}/export${query ? `?${query}` : ""}`,
    { method: "GET" },
  );
  if (error) return error;
  // Stream the response through unchanged so the browser gets the
  // Content-Disposition filename the Go server chose.
  const headers = new Headers();
  const ct = upstream.headers.get("Content-Type");
  if (ct) headers.set("Content-Type", ct);
  const cd = upstream.headers.get("Content-Disposition");
  if (cd) headers.set("Content-Disposition", cd);
  // The HTML export's body is model- and tool-authored text. It is served as an
  // attachment, and nosniff keeps the browser from re-typing it into something
  // it would render on this origin instead of saving.
  headers.set("X-Content-Type-Options", "nosniff");
  return new NextResponse(upstream.body, { status: upstream.status, headers });
}
