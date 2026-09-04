import { NextRequest, NextResponse } from "next/server";
import { getServerSession } from "@/app/lib/auth";
import { chatServerPassthrough } from "@/app/lib/chatServer";

export const runtime = "nodejs";

type Params = { params: Promise<{ projectId: string }> };

// The project home's Team section (ADR-0057): the chats other members of the
// team have shared into THIS project. Both gates — a shared team_id and each
// owner's per-chat opt-in — live in the Go handler
// (internal/httpapi/projects.go → projectTeamConversations); this proxy only
// authenticates and forwards.
export async function GET(_request: NextRequest, { params }: Params) {
  const session = await getServerSession();
  if (!session) return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  const { projectId } = await params;
  return chatServerPassthrough(
    session,
    `/projects/${encodeURIComponent(projectId)}/team-conversations`,
  );
}
