import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";
import { verifyOrigin } from "@/app/lib/csrf";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ conversationId: string }> };

// POST /api/conversations/{id}/outputs/share — the owner shares or unshares
// ONE output (body: { path, shared }). Answers the same body as GET outputs.
// Ownership and path validation live in the Go handler; this proxy only
// authenticates, checks the origin, and forwards.
export async function POST(request: NextRequest, context: RouteContext) {
  const csrf = verifyOrigin(request);
  if (!csrf.ok) return csrf.response;
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { conversationId } = await context.params;
  return chatServerPassthrough(
    session,
    `/conversations/${encodeURIComponent(conversationId)}/outputs/share`,
    { method: "POST", body: await request.text() },
  );
}
