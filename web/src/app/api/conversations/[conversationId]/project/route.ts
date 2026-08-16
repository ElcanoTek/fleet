import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ conversationId: string }> };

// Re-file a conversation into a project, or unfile it with an empty
// project_id (the rail's drag-a-chat-into-a-project flow). Membership checks
// live in the Go handler (internal/httpapi/server.go, `sub == "project"`),
// which 404s for missing and non-member projects alike.
export async function POST(request: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;

  const session = await getServerSession();
  if (!session) {
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }
  const { conversationId } = await context.params;
  return chatServerPassthrough(
    session,
    `/conversations/${encodeURIComponent(conversationId)}/project`,
    { method: "POST", body: await request.text() },
  );
}
