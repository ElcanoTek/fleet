import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ conversationId: string }> };

export async function GET(_request: NextRequest, context: RouteContext) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId } = await context.params;
  const { upstream, error } = await chatServerProxy(
    session.email,
    `/conversations/${encodeURIComponent(conversationId)}/export`,
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
  return new NextResponse(upstream.body, { status: upstream.status, headers });
}
