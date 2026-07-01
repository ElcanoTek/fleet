import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerProxy } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ conversationId: string }> };

// POST /api/conversations/{id}/promote-to-task → synthesizes a recurring-task
// proposal from the conversation and stages a schedule_task approval (#455).
// Proxies to the chat server's /conversations/{id}/promote-to-task; the JSON
// (staged approval card + rationale) passes through to the client, which then
// re-hydrates the conversation to render the pre-filled ScheduleTaskCard.
export async function POST(request: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId } = await context.params;
  const { upstream, error } = await chatServerProxy(
    session.email,
    `/conversations/${encodeURIComponent(conversationId)}/promote-to-task`,
    { method: "POST" },
  );
  if (error) return error;
  const text = await upstream.text();
  return new NextResponse(text, {
    status: upstream.status,
    headers: { "Content-Type": upstream.headers.get("Content-Type") ?? "application/json" },
  });
}
