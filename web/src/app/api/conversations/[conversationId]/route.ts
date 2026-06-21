import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerFetch } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ conversationId: string }> };

export async function GET(_: NextRequest, context: RouteContext) {
  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId } = await context.params;
  const upstream = await chatServerFetch(session.email, `/conversations/${encodeURIComponent(conversationId)}`, {
    method: "GET",
  });
  return passthrough(upstream);
}

export async function DELETE(request: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId } = await context.params;
  const upstream = await chatServerFetch(session.email, `/conversations/${encodeURIComponent(conversationId)}`, {
    method: "DELETE",
  });
  if (upstream.status === 204) {
    return NextResponse.json({ ok: true });
  }
  return passthrough(upstream);
}

async function passthrough(upstream: Response) {
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
