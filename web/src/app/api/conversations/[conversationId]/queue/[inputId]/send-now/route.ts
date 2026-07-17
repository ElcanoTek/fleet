import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = {
  params: Promise<{ conversationId: string; inputId: string }>;
};

// POST /api/conversations/{id}/queue/{inputId}/send-now — promote a queued
// input to the head of the queue; a running turn is also offered it at the
// next safe step boundary (#785).
export async function POST(request: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId, inputId } = await context.params;
  const { upstream, error } = await chatServerProxy(
    session.email,
    `/conversations/${encodeURIComponent(conversationId)}/queue/${encodeURIComponent(inputId)}/send-now`,
    { method: "POST" },
  );
  if (error) return error;
  if (upstream.status === 204) {
    return NextResponse.json({ ok: true });
  }
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": "application/json" },
  });
}
