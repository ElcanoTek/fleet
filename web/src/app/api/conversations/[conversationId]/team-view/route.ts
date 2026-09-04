import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type Params = { params: Promise<{ conversationId: string }> };

// Read-only transcript of a teammate's team-shared chat (ADR-0057). The two
// gates — a shared team_id and the owner's per-chat opt-in — are enforced in
// the store; a chat the caller may not read comes back 404, indistinguishable
// from one that does not exist.
export async function GET(_request: NextRequest, { params }: Params) {
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { conversationId } = await params;
  return chatServerPassthrough(
    session,
    `/conversations/${encodeURIComponent(conversationId)}/team-view`,
  );
}
