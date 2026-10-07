import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ conversationId: string }> };

// GET /api/conversations/{id}/team-link — where a team link should land the
// signed-in caller: owner | open | not_on_team | not_shared (ADR-0079). The
// Go handler decides and reveals nothing beyond the status; this proxy only
// authenticates and forwards. (A signed-out visitor never reaches it — the
// proxy middleware sends them to sign in first.)
export async function GET(_request: NextRequest, context: RouteContext) {
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { conversationId } = await context.params;
  return chatServerPassthrough(
    session,
    `/conversations/${encodeURIComponent(conversationId)}/team-link`,
  );
}
