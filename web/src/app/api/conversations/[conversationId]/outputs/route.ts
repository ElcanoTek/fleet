import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type RouteContext = { params: Promise<{ conversationId: string }> };

// GET /api/conversations/{id}/outputs — the owner's list of this chat's
// outputs (files the agent presented in its replies; uploads never) with each
// file's own share state (docs/TEAM-SHARING.md, ADR-0079). Owner-only; the
// gate and the definition of an output live in the Go handler
// (internal/httpapi/team_files.go → handleConversationOutputs).
export async function GET(_request: NextRequest, context: RouteContext) {
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { conversationId } = await context.params;
  return chatServerPassthrough(
    session,
    `/conversations/${encodeURIComponent(conversationId)}/outputs`,
  );
}
